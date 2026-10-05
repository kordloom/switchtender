package relay_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// audDisconnectingRuns is the control node's run store, which lets the test drop the worker's
// connection at one exact point: just after the terminal report has finalized the run and before
// the handler commits the run's outcome to the chain.
type audDisconnectingRuns struct {
	run.Store
	run.SummaryAppender
	// finalized is closed once a terminal report has moved the run.
	finalized chan struct{}
	// once makes only the first terminal report wait for the disconnect.
	once sync.Once
}

// FinalizeRunning finalizes the run and, for the first terminal report, waits until the worker's
// request is gone before returning to the handler.
func (s *audDisconnectingRuns) FinalizeRunning(ctx context.Context, id string,
	fin run.Finalization) (bool, error) {
	moved, err := s.Store.FinalizeRunning(ctx, id, fin)
	if err == nil && moved && fin.Status.Terminal() {
		s.once.Do(func() {
			close(s.finalized)
			<-ctx.Done()
		})
	}
	return moved, err
}

// TestChainKeepsTheRelayOutcomeWhenTheWorkerDisconnects finishes a relay run whose worker loses its
// connection while the control node is processing the terminal report, the way a worker restarted
// by its supervisor, a load balancer idle timeout, or a network blip ends a request, and then lets
// the worker report again.
//
// The control node finalizes the run in the store and only then commits its outcome, on the
// worker's request context, and a failed commit is logged and dropped. The context is gone by then,
// so the commit fails, and the worker's retry is refused with 409 because the run is already
// terminal. Nothing else ever commits it, so a run that executed on real hosts and is succeeded in
// the store has no outcome anywhere in the chain, and its receipt verifies while saying nothing
// about what it did. The worker's report is the one input the control node does not control, and
// losing it here takes no fault at all on the control node's side.
func TestChainKeepsTheRelayOutcomeWhenTheWorkerDisconnects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runs := db.Runs()
	appender, ok := runs.(run.SummaryAppender)
	if !ok {
		t.Fatal("the SQLite run store does not append summaries")
	}
	store := &audDisconnectingRuns{Store: runs, SummaryAppender: appender,
		finalized: make(chan struct{})}
	ts := httptest.NewServer(relay.NewHandler(store, relay.SinglePool(testWorkerToken), nil, nil,
		db.Audits()))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, nil))

	const id = "run_relay_disconnect"
	if err := runs.Save(ctx, &run.Run{
		ID: id, Playbook: "site.yml", Inventory: "prod", Status: run.StatusPending,
		Actor: "alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	claimed, err := c.Claim(ctx, "worker-a", []string{""})
	if err != nil || claimed == nil {
		t.Fatalf("Claim() = %v, %v", claimed, err)
	}
	if err := c.AppendLog(ctx, id, []byte("ok: [web01]\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	report, drop := context.WithCancel(ctx)
	go func() {
		<-store.finalized
		drop()
	}()
	claimed.Status = run.StatusSucceeded
	if err := c.Save(report, claimed); err == nil {
		t.Log("the dropped terminal report returned no error to the worker")
	}
	// The worker reports again on a fresh request, as a worker does when a report fails.
	if err := c.Save(ctx, claimed); err != nil {
		t.Logf("the worker's retried terminal report: %v", err)
	}

	got, err := runs.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusSucceeded {
		t.Fatalf("the run is %s in the store, want succeeded", got.Status)
	}
	chain, err := db.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
			return
		}
	}
	t.Errorf("run %s is succeeded in the control node's store and the chain holds no outcome "+
		"for it: the worker's connection dropping mid-report lost the record of what it did", id)
}
