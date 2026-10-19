package relay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// summaryStore is a run store that also appends summaries, which the relay handler requires.
type summaryStore interface {
	run.Store
	run.SummaryAppender
}

// fullDiskStore is a run store whose log appends all fail, standing in for a control node whose
// database disk filled while a worker was streaming a run's output to it.
type fullDiskStore struct {
	summaryStore
}

// AppendLog refuses every append the way a full disk does.
func (fullDiskStore) AppendLog(context.Context, string, []byte) error {
	return errors.New("database or disk is full (13)")
}

// logIncomplete is the failure a run carries when its output was not stored in full, as the API
// shows it.
func logIncomplete(lost int) string {
	return fmt.Sprintf("the run's output was not stored in full: %d bytes could not be saved, so its "+
		"log cannot show everything the run did", lost)
}

// waitTerminalFor is waitTerminal with its own budget, for a run whose end waits out the transport's
// terminal flush window first.
func waitTerminalFor(t *testing.T, store run.Store, id string, budget time.Duration) *run.Run {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		got, err := store.Get(context.Background(), id)
		if err == nil && got.Status.Terminal() {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached a terminal status", id)
	return nil
}

// TestRelayRunWhoseOutputWasNotStoredFails pins that a run executed across the relay cannot finish
// succeeded when the control node refused its output. The worker's transport answers every append
// before it lands, so the dispatcher asks it to settle the log before deciding the outcome, and the
// failure goes out in the terminal save the worker already sends, so the committed outcome names the
// gap in the log it digests. The test waits out the transport's terminal flush window, which is the
// time the tail is given to land.
func TestRelayRunWhoseOutputWasNotStoredFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem, ok := run.NewMemStore().(summaryStore)
	if !ok {
		t.Fatal("the memory store does not append summaries")
	}
	backing := fullDiskStore{summaryStore: mem}
	audits := audit.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)

	client := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, nil))
	worker := dispatch.New(client, recapRunner{}, zaptest.NewLogger(t),
		dispatch.WithWorkers(1), dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
		dispatch.WithClaimInterval(10*time.Millisecond))
	t.Cleanup(worker.Close)

	if err := backing.Save(ctx, &run.Run{
		ID: "run_relay_lost", Playbook: "site.yml", Inventory: "prod",
		Status: run.StatusPending, Actor: "alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	finished := waitTerminalFor(t, backing, "run_relay_lost", 45*time.Second)
	if finished.Status != run.StatusFailed {
		t.Fatalf("run status = %q, want failed: the control node kept none of its output", finished.Status)
	}
	want := logIncomplete(len("PLAY RECAP\n"))
	if finished.Error != want {
		t.Errorf("run error = %q, want %q", finished.Error, want)
	}

	body, err := outcome.Body(ctx, backing, finished)
	if err != nil {
		t.Fatalf("outcome.Body() error = %v", err)
	}
	entry := outcomeEntry(t, audits, "run_relay_lost")
	if !outcome.VerifyBody(entry.ContentDigest, entry.Nonce, body) {
		t.Fatal("the committed digest does not verify against the outcome body")
	}
	rec, err := outcome.Parse(body)
	if err != nil {
		t.Fatalf("outcome.Parse() error = %v", err)
	}
	if rec.Error != want {
		t.Errorf("committed failure reason = %q, want %q", rec.Error, want)
	}
}

// TestRelayRunWhoseOutputOverflowedTheWorkerFails pins the loss that never showed at all. While a
// relay is down the worker holds a run's output up to a cap and drops the oldest past it. Once the
// relay returned the end of the log landed, the terminal save went out clean, and the run was
// recorded succeeded over a log missing its middle, with no warning anywhere, because nothing counted
// what the cap removed. The run now fails, and the reason names exactly the bytes the control node
// does not have, which the test measures as what the run wrote less what the store holds.
func TestRelayRunWhoseOutputOverflowedTheWorkerFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	audits := audit.NewMemStore()

	// A control node whose log endpoint is down for the whole run and back by the time it ends.
	var down atomic.Bool
	down.Store(true)
	inner := relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, audits)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") && down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	// Ten batches of output, more than the worker holds for one run while posts are failing.
	const chunk = 64 << 10
	var written atomic.Int64
	runner := roundhouse.RunnerFunc(func(_ context.Context, _ roundhouse.Spec,
		out io.Writer) (roundhouse.Result, error) {
		for i := range 10 {
			n, err := out.Write(bytes.Repeat([]byte{byte('0' + i)}, chunk))
			written.Add(int64(n))
			if err != nil {
				return roundhouse.Result{ExitCode: 1}, err
			}
		}
		down.Store(false)
		return roundhouse.Result{ExitCode: 0}, nil
	})
	client := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))
	worker := dispatch.New(client, runner, zaptest.NewLogger(t),
		dispatch.WithWorkers(1), dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
		dispatch.WithClaimInterval(10*time.Millisecond))
	t.Cleanup(worker.Close)

	if err := backing.Save(ctx, &run.Run{
		ID: "run_relay_overflow", Playbook: "site.yml", Inventory: "prod",
		Status: run.StatusPending, Actor: "alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	finished := waitTerminal(t, backing, "run_relay_overflow")
	stored, err := backing.Log(ctx, "run_relay_overflow")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	lost := int(written.Load()) - len(stored)
	if lost <= 0 {
		t.Fatalf("the store holds %d of %d bytes written, so the cap never applied and the test "+
			"proves nothing", len(stored), written.Load())
	}
	if finished.Status != run.StatusFailed {
		t.Fatalf("run status = %q, want failed: %d bytes of its output never reached the control node",
			finished.Status, lost)
	}
	if want := logIncomplete(lost); finished.Error != want {
		t.Errorf("run error = %q, want %q", finished.Error, want)
	}
}
