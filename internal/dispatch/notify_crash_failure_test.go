package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// obxCrashAudits is a chain whose first outcome append never returns until released, standing in
// for a process killed after a run's terminal record landed and before its outcome was committed.
type obxCrashAudits struct {
	audit.Store
	// entered is closed when the outcome append arrives.
	entered chan struct{}
	// release is closed when the test is done with the stopped process.
	release chan struct{}
	// once holds only the first outcome append.
	once sync.Once
}

// Append holds the first outcome entry until released, then appends every entry.
func (c *obxCrashAudits) Append(ctx context.Context, e *audit.Entry) error {
	if strings.Contains(e.Path, "/outcome/") {
		c.once.Do(func() {
			close(c.entered)
			<-c.release
		})
	}
	return c.Store.Append(ctx, e)
}

// TestOutboxAnnouncesARunWhoseEndOutlivedACrash covers a process killed at the step between a run's
// terminal record and the rest of its end. The executor writes the run's terminal status, then
// commits its outcome to the chain, and only then records the event for the run's named targets.
// When the process dies after the first write, the run is finished in the database, the target
// heard it start, and nothing ever records its end: the restarted process sees a terminal run, so
// neither its claim loop nor its janitor has anything to do with it, and no sweep looks for a
// finished run whose end was never announced. Each event is promised to be recorded as the run
// reaches it, and a delivery pending when a server stops is made by the next process, but this
// event never becomes a delivery at all, and the run's outcome entry is missing from the chain for
// the same reason.
func TestOutboxAnnouncesARunWhoseEndOutlivedACrash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recv := &scriptedReceiver{}
	srv := httptest.NewServer(http.HandlerFunc(recv.serve))
	t.Cleanup(srv.Close)
	targets := obxAttachedHook(t, srv.URL+"/hook")
	outbox := func() *named.Outbox {
		router := named.NewRouter(targets, plainSealer{}, named.SourceLineage(nil, nil, nil), nil)
		return named.NewOutbox(targets, router, plainSealer{}, nil,
			named.WithPoll(10*time.Millisecond))
	}
	runs := run.NewMemStore()
	chain := audit.NewMemStore()
	crash := &obxCrashAudits{Store: chain, entered: make(chan struct{}),
		release: make(chan struct{})}

	first := New(runs, okRunner(), nil, WithAudits(crash), WithNotificationOutbox(outbox()),
		WithNotifyClient(http.DefaultClient))
	t.Cleanup(func() {
		close(crash.release)
		first.Close()
	})
	created, err := first.Submit(ctx, "", "", run.WithTool(run.ToolBash),
		run.WithCommand("echo hi"), run.WithSource("template", "tpl_named"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-crash.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached its outcome commit")
	}
	got, err := runs.Get(ctx, created.ID)
	if err != nil || got.Status != run.StatusSucceeded {
		t.Fatalf("the run's stored record at the crash = %+v, %v, want succeeded", got, err)
	}

	// The first process is gone from here on. A new one starts on the same database.
	restarted := New(runs, okRunner(), nil, WithAudits(chain), WithNotificationOutbox(outbox()),
		WithNotifyClient(http.DefaultClient))
	t.Cleanup(restarted.Close)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := targets.Deliveries(ctx, named.DeliveryFilter{RunID: created.ID})
		if err != nil {
			t.Fatalf("Deliveries() error = %v", err)
		}
		for _, dl := range list {
			if dl.Event == named.EventSuccess {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _ := targets.Deliveries(ctx, named.DeliveryFilter{RunID: created.ID})
	var events []string
	for _, dl := range list {
		events = append(events, dl.Event+" "+dl.Status)
	}
	t.Fatalf("the run finished in the database before its process died, and after a restart its "+
		"targets were told only %v: its end was never recorded", events)
}

// obxFlakyRecords is a notification store whose first record of a run's end fails, standing in for
// a database that drops one connection, fails over, or times out a statement at that moment.
type obxFlakyRecords struct {
	named.Store
	// failed reports that the one failure has been served.
	failed atomic.Bool
}

// Record fails the first time it is asked to record a success or a failure, and records everything
// else.
func (f *obxFlakyRecords) Record(ctx context.Context, ev *named.RunEvent,
	recipients []named.Recipient) (bool, error) {
	if (ev.Event == named.EventSuccess || ev.Event == named.EventFailure) &&
		f.failed.CompareAndSwap(false, true) {
		return false, errors.New("connection reset by peer")
	}
	return f.Store.Record(ctx, ev, recipients)
}

// TestOutboxRecordsAnEventAfterATransientStoreError covers the same step as the crash above with
// the process alive. The run's terminal record is saved with retries, so a brief database fault
// does not lose the outcome, and the event for its named targets is recorded once, with no retry:
// one failed statement at that moment and the event is logged and dropped. The run finished, the
// target heard it start, and no delivery of its end ever exists, although the database was
// reachable a moment later.
func TestOutboxRecordsAnEventAfterATransientStoreError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recv := &scriptedReceiver{}
	srv := httptest.NewServer(http.HandlerFunc(recv.serve))
	t.Cleanup(srv.Close)
	targets := &obxFlakyRecords{Store: obxAttachedHook(t, srv.URL+"/hook")}
	router := named.NewRouter(targets, plainSealer{}, named.SourceLineage(nil, nil, nil), nil)
	outbox := named.NewOutbox(targets, router, plainSealer{}, nil,
		named.WithPoll(10*time.Millisecond))
	d := New(run.NewMemStore(), okRunner(), nil, WithAudits(audit.NewMemStore()),
		WithNotificationOutbox(outbox), WithNotifyClient(http.DefaultClient))
	t.Cleanup(d.Close)
	created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash),
		run.WithCommand("echo hi"), run.WithSource("template", "tpl_named"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitUntil(t, "the run to finish", func() bool {
		got, err := d.store.Get(ctx, created.ID)
		return err == nil && got.Status == run.StatusSucceeded
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := targets.Deliveries(ctx, named.DeliveryFilter{RunID: created.ID})
		if err != nil {
			t.Fatalf("Deliveries() error = %v", err)
		}
		for _, dl := range list {
			if dl.Event == named.EventSuccess {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	hits, _ := recv.snapshot()
	t.Fatalf("one failed statement while recording the run's end, and the target attached for "+
		"success was never told: received %+v", hits)
}
