package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestOutboxShutdownDoesNotSpendTheLastAttempt covers a server stopping while a delivery is on its
// last retry. The delivery failed four times with an answer the target gave, and the fifth attempt
// is in flight when the process shuts down, so the attempt is cut short by the process rather than
// refused by the target. The documented promise is that a delivery pending when a server stops is
// made by the next process to start on it. The outbox counts the canceled attempt as the fifth
// failure and marks the delivery failed for good with "context canceled", so the next process never
// tries it and every later message to that target about that run carries the note that an earlier
// one could not be delivered. A rolling restart is enough to make it happen.
func TestOutboxShutdownDoesNotSpendTheLastAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(t, "ntf_a")
	failing := errors.New("the target answered 503")
	sender := &recordingSender{script: map[string][]error{
		"https://hooks.example.com/ntf_a": {failing, failing, failing, failing},
	}}
	if err := f.outbox.Record(ctx, runAt("run_obx_stop", run.StatusRunning), Branch{}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	drain(t, f, sender)
	for attempt := 2; attempt < MaxAttempts; attempt++ {
		f.clock.advance(RetryDelay(attempt - 1))
		drain(t, f, sender)
	}
	f.clock.advance(RetryDelay(MaxAttempts - 1))

	// The last attempt is in flight when the server stops, which cancels it.
	stopping, stop := context.WithCancel(ctx)
	defer stop()
	var once sync.Once
	inFlight := func(c context.Context, _ Message) error {
		once.Do(stop)
		<-c.Done()
		return c.Err()
	}
	if n := f.outbox.Flush(stopping, inFlight); n != 1 {
		t.Fatalf("the shutdown pass claimed %d deliveries, want the one on its last attempt", n)
	}

	// The next process starts and delivers whatever is still pending, against a target that is up.
	f.clock.advance(RetryDelay(MaxAttempts))
	drain(t, f, &recordingSender{script: map[string][]error{}})
	list, err := f.store.Deliveries(ctx, DeliveryFilter{RunID: "run_obx_stop"})
	if err != nil || len(list) != 1 {
		t.Fatalf("Deliveries() = %v, %v, want one", list, err)
	}
	got := fmt.Sprintf("%s %s", list[0].Status, list[0].LastError)
	if diff := cmp.Diff(DeliveryDelivered+" ", got); diff != "" {
		t.Errorf("a delivery whose last attempt the shutdown cut short was given up on rather "+
			"than left for the next process (-want +got):\n%s", diff)
	}
}

// TestOutboxNeverPagesAHoldThroughARetypedTarget covers a target edited while a delivery to it is
// queued. The API lets an edit change a target's kind. A hold is recorded for a webhook target,
// which hears holds, and before it is delivered the target is changed to PagerDuty, the kind a hold
// must never reach. The kind rule is applied when the event is recorded and never again, and the
// delivery opens the target as it stands at the attempt, so the queued hold is handed to the sender
// as a PagerDuty message, which the dispatcher turns into a triggered incident: a page for a run
// that is only waiting for a person. The documented rule is that a hold reaches a PagerDuty,
// Grafana, or Twilio target never, and that PagerDuty pages only for a run that failed or was
// interrupted.
func TestOutboxNeverPagesAHoldThroughARetypedTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	mustTarget(t, store, "ntf_obx_retyped", run.NotifyTarget{Kind: run.NotifyWebhook,
		URL: "https://hooks.example.com/ntf_obx_retyped"})
	for _, ev := range Events {
		mustAttach(t, store, "ntf_obx_retyped", KindTemplate, "tpl_deploy", ev)
	}
	o := NewOutbox(store, NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil),
		testSealer{}, nil)
	for _, status := range []run.Status{run.StatusPendingApproval, run.StatusSucceeded} {
		if err := o.Record(ctx, runAt("run_obx_held", status), Branch{}); err != nil {
			t.Fatalf("Record(%s) error = %v", status, err)
		}
	}

	// The target is edited to another kind while both deliveries wait.
	n, err := store.Get(ctx, "ntf_obx_retyped")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if err := n.SetTarget(run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "pd-routing-key"},
		testSealer{}); err != nil {
		t.Fatalf("SetTarget() error = %v", err)
	}
	if err := store.Update(ctx, n); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	var mu sync.Mutex
	var paged []string
	send := func(_ context.Context, m Message) error {
		mu.Lock()
		defer mu.Unlock()
		if m.Target.Kind == run.NotifyPagerDuty && !Hears(m.Target.Kind, m.Run) {
			paged = append(paged, fmt.Sprintf("%s #%d", m.Run.Status, m.Seq))
		}
		return nil
	}
	for range 10 {
		if o.Flush(ctx, send) == 0 {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paged) > 0 {
		t.Errorf("queued events a PagerDuty target must never hear were handed to it after the "+
			"target was retyped: %v", paged)
	}
}

// TestOutboxTargetsNeverWaitForEachOther covers one target hanging while another is healthy. The
// documented promise is that targets never wait for each other, and the outbox's own comment says
// the per-pass worker bound exists so one slow target does not hold up every other target's
// messages. A pass claims a batch, attempts it through eight slots, and does not claim again until
// every attempt in the batch has ended. A target that accepts the connection and never answers, a
// firewall that drops packets or a chat service in an outage, holds each slot it is given until the
// attempt times out, five seconds for a webhook and fifteen for email, so the eight slots fill with
// its attempts and the healthy target's messages, due at the same moment, wait behind them. With a
// backlog for the hung target queued ahead, a healthy target is told minutes late.
func TestOutboxTargetsNeverWaitForEachOther(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(t, "ntf_dead", "ntf_live")
	const runs = 20
	for i := range runs {
		r := runAt(fmt.Sprintf("run_obx_%02d", i), run.StatusRunning)
		if err := f.outbox.Record(ctx, r, Branch{}); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}
	hung := make(chan struct{})
	var stuck, live atomic.Int64
	send := func(c context.Context, m Message) error {
		if strings.HasSuffix(m.Target.URL, "/ntf_dead") {
			stuck.Add(1)
			select {
			case <-hung:
				return errors.New("the target answered 503")
			case <-c.Done():
				return c.Err()
			}
		}
		live.Add(1)
		return nil
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
	for live.Load() < runs && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := live.Load(); got < runs {
		t.Errorf("the healthy target was told %d of the %d events due to it, while %d attempts "+
			"at a hung target held the outbox", got, runs, stuck.Load())
	}
}
