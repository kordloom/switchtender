package dispatch_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// endsSealer seals by prefixing, which is all the test needs from the encryption key.
type endsSealer struct{}

// Enabled reports that the sealer has a key.
func (endsSealer) Enabled() bool { return true }

// Seal prefixes the plaintext.
func (endsSealer) Seal(plain string) (string, error) { return "sealed:" + plain, nil }

// Open strips the prefix.
func (endsSealer) Open(sealed string) (string, error) {
	plain, ok := strings.CutPrefix(sealed, "sealed:")
	if !ok {
		return "", errors.New("not sealed")
	}
	return plain, nil
}

// TestOwedEndsReachNamedTargetsOnSQLite pins, against a real database, that a run's end reaches
// the named targets attached for it whichever write ended the run, including writes no announcing
// path followed: a finalize whose process stopped before announcing, a cancel nothing announced,
// and a decision recorded by its status change alone. The database marks each end owed in the same
// statement as the status change, and the sweep of owed ends records it, once, and settles it.
func TestOwedEndsReachNamedTargetsOnSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)
	targets := db.Notifications()
	n := &notification.Notification{ID: "ntf_ends", Name: "ends", CreatedAt: time.Now()}
	if err := n.SetTarget(run.NotifyTarget{Kind: run.NotifyWebhook, URL: hook.URL + "/hook"},
		endsSealer{}); err != nil {
		t.Fatalf("SetTarget() error = %v", err)
	}
	if err := targets.Save(ctx, n); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, ev := range notification.Events {
		if err := targets.Attach(ctx, &notification.Attachment{ID: notification.NewAttachmentID(),
			NotificationID: n.ID, ObjectKind: notification.KindTemplate, ObjectID: "tpl_ends",
			Event: ev, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
	}
	router := notification.NewRouter(targets, endsSealer{},
		notification.SourceLineage(nil, nil, nil), nil)
	outbox := notification.NewOutbox(targets, router, endsSealer{}, nil,
		notification.WithPoll(10*time.Millisecond))
	runs := db.Runs()
	idle := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (
		roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})
	d := dispatch.New(runs, idle, nil, dispatch.WithNotificationOutbox(outbox),
		dispatch.WithNotifyClient(http.DefaultClient))
	t.Cleanup(d.Close)

	now := time.Now().UTC()
	tests := []struct {
		Name      string
		Start     *run.Run
		End       func() (bool, error)
		WantEvent string
	}{{ // Test 0: A finalize whose process stopped before it announced the end.
		Name: "finalized",
		Start: &run.Run{ID: "run_ends_final", Status: run.StatusRunning, ClaimedBy: "gone",
			ClaimedAt: &now, StartedAt: &now},
		End: func() (bool, error) {
			return runs.FinalizeRunning(ctx, "run_ends_final", run.Finalization{
				Status: run.StatusSucceeded, EndedAt: now, Owner: "gone"})
		},
		WantEvent: notification.EventSuccess,
	}, { // Test 1: A cancel of a queued run that nothing announced.
		Name:  "canceled",
		Start: &run.Run{ID: "run_ends_cancel", Status: run.StatusPending, Queue: "unserved"},
		End: func() (bool, error) {
			return runs.CancelPending(ctx, "run_ends_cancel")
		},
		WantEvent: notification.EventFailure,
	}, { // Test 2: A rejection recorded by its status change alone.
		Name:  "rejected",
		Start: &run.Run{ID: "run_ends_reject", Status: run.StatusPendingApproval},
		End: func() (bool, error) {
			return runs.TransitionStatus(ctx, "run_ends_reject", run.StatusPendingApproval,
				run.StatusRejected)
		},
		WantEvent: notification.EventFailure,
	}}
	for _, test := range tests {
		test.Start.Playbook, test.Start.CreatedAt = "site.yml", now
		test.Start.Source, test.Start.SourceID = "template", "tpl_ends"
		if err := runs.Save(ctx, test.Start); err != nil {
			t.Fatalf("%s: Save() error = %v", test.Name, err)
		}
		if moved, err := test.End(); err != nil || !moved {
			t.Fatalf("%s: ending the run moved = %v, error = %v", test.Name, moved, err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for waiting := true; waiting; {
		waiting = false
		for testNum, test := range tests {
			list, err := targets.Deliveries(ctx, notification.DeliveryFilter{RunID: test.Start.ID})
			if err != nil {
				t.Fatalf("test %d: Deliveries() error = %v", testNum, err)
			}
			var ends []string
			for _, dl := range list {
				if notification.IsEnd(dl.Event) {
					ends = append(ends, dl.Event+" "+dl.Status)
				}
			}
			done := len(ends) == 1 && ends[0] == test.WantEvent+" "+notification.DeliveryDelivered
			if len(ends) > 1 || (!done && time.Now().After(deadline)) {
				t.Fatalf("test %d %s: the end of %s reached its target as %v, want one %s "+
					"delivered", testNum, test.Name, test.Start.ID, ends, test.WantEvent)
			}
			waiting = waiting || !done
		}
		time.Sleep(20 * time.Millisecond)
	}
	ledger, ok := runs.(run.EndLedger)
	if !ok {
		t.Fatal("the SQLite run store keeps no ledger of owed ends")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		owed, err := ledger.OwedEnds(ctx, 0, 0)
		if err == nil && len(owed) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ends still owed after they were recorded: %v, %v", owed, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
