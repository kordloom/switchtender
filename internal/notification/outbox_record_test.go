package notification

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// flakyStore is a Store whose attachment reads and event writes fail a set number of times before
// they work, standing in for a database that drops a connection or fails over.
type flakyStore struct {
	Store
	// mu guards the counts.
	mu sync.Mutex
	// failReads is how many attachment reads fail before they work, or every one when negative.
	failReads int
	// failWrites is how many event writes fail before they work.
	failWrites int
}

// AttachedTo fails while reads are set to fail, then reads the wrapped store.
func (f *flakyStore) AttachedTo(ctx context.Context, kind, objectID string) ([]*Attachment, error) {
	f.mu.Lock()
	fail := f.failReads != 0
	if f.failReads > 0 {
		f.failReads--
	}
	f.mu.Unlock()
	if fail {
		return nil, errors.New("connection reset by peer")
	}
	return f.Store.AttachedTo(ctx, kind, objectID)
}

// Record fails while writes are set to fail, then records in the wrapped store.
func (f *flakyStore) Record(ctx context.Context, ev *RunEvent,
	recipients []Recipient) (bool, error) {
	f.mu.Lock()
	fail := f.failWrites > 0
	if fail {
		f.failWrites--
	}
	f.mu.Unlock()
	if fail {
		return false, errors.New("the statement was canceled by a failover")
	}
	return f.Store.Record(ctx, ev, recipients)
}

// TestOutboxRecordSurvivesAStoreThatFailsForAMoment pins that an event is not lost to a moment of
// database trouble: a refused write or an unreadable attachment is asked again within the record
// bound and the event is recorded, and a store that stays unreadable is reported as an error rather
// than as an event recorded for fewer targets, or none, which a caller would count as told.
func TestOutboxRecordSurvivesAStoreThatFailsForAMoment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		FailReads  int
		FailWrites int
		WantErr    bool
		WantKeys   []string
	}{{ // Test 0: The attachments cannot be read once.
		Name: "attachments unreadable once", FailReads: 1,
		WantKeys: []string{"ntf_a/run_flaky/1"},
	}, { // Test 1: The event write is refused twice.
		Name: "write refused twice", FailWrites: 2,
		WantKeys: []string{"ntf_a/run_flaky/1"},
	}, { // Test 2: The attachments stay unreadable.
		Name: "attachments unreadable throughout", FailReads: -1, WantErr: true,
		WantKeys: []string{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			base := NewMemStore()
			mustTarget(t, base, "ntf_a", run.NotifyTarget{Kind: run.NotifyWebhook,
				URL: "https://hooks.example.com/ntf_a"})
			mustAttach(t, base, "ntf_a", KindTemplate, "tpl_deploy", EventSuccess)
			store := &flakyStore{Store: base, failReads: test.FailReads,
				failWrites: test.FailWrites}
			o := NewOutbox(store, NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil),
				testSealer{}, nil)
			err := o.Record(ctx, runAt("run_flaky", run.StatusSucceeded), Branch{})
			if (err != nil) != test.WantErr {
				t.Errorf("Record() error = %v, want error %v", err, test.WantErr)
			}
			list, lerr := base.Deliveries(ctx, DeliveryFilter{RunID: "run_flaky"})
			if lerr != nil {
				t.Fatalf("Deliveries() error = %v", lerr)
			}
			got := []string{}
			for _, d := range list {
				got = append(got, d.Key())
			}
			if diff := cmp.Diff(test.WantKeys, got); diff != "" {
				t.Errorf("recorded deliveries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOutboxAppliesTheChannelRulesToTheTargetAsItStands pins that a delivery is judged against the
// target it would reach, not the one the event was recorded for. A target edited to a pager, a text
// recipient, or a dashboard is never handed a hold, and a pager never a success, which are recorded
// as skipped with why; what the new kind does hear still goes.
func TestOutboxAppliesTheChannelRulesToTheTargetAsItStands(t *testing.T) {
	t.Parallel()
	alert := &run.AttentionNote{ID: "att_1", Blocker: "approval_needed", Summary: "waited"}
	tests := []struct {
		Name       string
		Status     run.Status
		Attention  *run.AttentionNote
		Retyped    run.NotifyTarget
		WantStatus string
		WantSent   bool
	}{{ // Test 0: A hold, retyped to a text recipient.
		Name: "hold to twilio", Status: run.StatusPendingApproval,
		Retyped:    run.NotifyTarget{Kind: run.NotifyTwilio, To: "+15550000000"},
		WantStatus: DeliverySkipped,
	}, { // Test 1: A hold, retyped to a dashboard.
		Name: "hold to grafana", Status: run.StatusPendingApproval,
		Retyped: run.NotifyTarget{Kind: run.NotifyGrafana, URL: "https://grafana.example.com",
			Key: "tok"},
		WantStatus: DeliverySkipped,
	}, { // Test 2: A success, retyped to a pager.
		Name: "success to pagerduty", Status: run.StatusSucceeded,
		Retyped:    run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "rk"},
		WantStatus: DeliverySkipped,
	}, { // Test 3: A failure, retyped to a pager, which hears it.
		Name: "failure to pagerduty", Status: run.StatusFailed,
		Retyped:    run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "rk"},
		WantStatus: DeliveryDelivered, WantSent: true,
	}, { // Test 4: An alert on a held run, retyped to a pager, which hears an alert.
		Name: "alert to pagerduty", Status: run.StatusPendingApproval, Attention: alert,
		Retyped:    run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "rk"},
		WantStatus: DeliveryDelivered, WantSent: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			mustTarget(t, store, "ntf_r", run.NotifyTarget{Kind: run.NotifyWebhook,
				URL: "https://hooks.example.com/ntf_r"})
			for _, ev := range Events {
				mustAttach(t, store, "ntf_r", KindTemplate, "tpl_deploy", ev)
			}
			o := NewOutbox(store, NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil),
				testSealer{}, nil)
			r := runAt("run_r", test.Status)
			r.Attention = test.Attention
			if err := o.Record(ctx, r, Branch{}); err != nil {
				t.Fatalf("Record() error = %v", err)
			}
			n, err := store.Get(ctx, "ntf_r")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if err := n.SetTarget(test.Retyped, testSealer{}); err != nil {
				t.Fatalf("SetTarget() error = %v", err)
			}
			if err := store.Update(ctx, n); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			sent := false
			o.Flush(ctx, func(_ context.Context, m Message) error {
				sent = true
				return nil
			})
			list, err := store.Deliveries(ctx, DeliveryFilter{RunID: "run_r"})
			if err != nil || len(list) != 1 {
				t.Fatalf("Deliveries() = %v, %v, want one", list, err)
			}
			if list[0].Status != test.WantStatus || sent != test.WantSent {
				t.Errorf("delivery = %s (%q), sent %v, want %s, sent %v", list[0].Status,
					list[0].LastError, sent, test.WantStatus, test.WantSent)
			}
		})
	}
}

// TestOutboxHungTargetHoldsOnlyItsShare pins the per-target share with a hung target whose backlog
// is larger than every attempt the outbox may make at once: it is attempted only up to its share,
// and the healthy target's deliveries keep going beside it rather than waiting for slots the hung
// target's attempts fill.
func TestOutboxHungTargetHoldsOnlyItsShare(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(t, "ntf_dead", "ntf_live")
	f.outbox.workers, f.outbox.perTarget = 8, 2
	const runs = 30
	for i := range runs {
		if err := f.outbox.Record(ctx, runAt(fmt.Sprintf("run_share_%02d", i), run.StatusRunning),
			Branch{}); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}
	hung := make(chan struct{})
	var mu sync.Mutex
	inFlight, most, live := 0, 0, 0
	send := func(c context.Context, m Message) error {
		mu.Lock()
		if m.Target.URL != "https://hooks.example.com/ntf_dead" {
			live++
			mu.Unlock()
			return nil
		}
		inFlight++
		most = max(most, inFlight)
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
		select {
		case <-hung:
			return errors.New("the target answered 503")
		case <-c.Done():
			return c.Err()
		}
	}
	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		f.outbox.Flush(ctx, send)
	}()
	defer func() {
		close(hung)
		<-passDone
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := live == runs
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if live != runs || most > 2 {
		t.Errorf("the healthy target was told %d of %d events while the hung one held %d "+
			"attempts at once, want all of them told and at most its share of 2 held", live, runs,
			most)
	}
}
