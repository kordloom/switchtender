package relay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestATerminalSaveWaitsForTheLogTail covers the end of a run's output going missing while its evidence
// says nothing is wrong.
//
// A worker buffers output and posts it in batches. When the run finishes it flushes what is left and
// then posts the terminal status. Those two were independent: a flush that failed put its bytes back and
// the terminal save went out anyway. Once the terminal save lands, the control node answers every later
// log append for that run with a silent no-op, by design, because a finished run is not a worker's to
// add to. So the retry mechanism that exists to deliver the tail could never succeed after the save, and
// the worker read the no-op as success and dropped the bytes.
//
// The lost part is the end: the play recap, the failure detail, the reason somebody is reading the log at
// all. And the control node commits the run's outcome as soon as the save lands, with a digest over the
// log it has, so the receipt attests a truncated log as complete. A two-second blip was enough.
func TestATerminalSaveWaitsForTheLogTail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()

	// A control node whose log endpoint refuses for a moment and then recovers, the way a restart or a
	// brief network fault does.
	var refuseUntil atomic.Int64
	refuseUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
	inner := relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") && time.Now().UnixNano() < refuseUntil.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	claimed := time.Now()
	r := &run.Run{
		ID: "run_tail", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	const recap = "PLAY RECAP *** ok=12 changed=3 failed=1"
	if err := c.AppendLog(ctx, r.ID, []byte(recap+"\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	// The run finishes. This is the moment the record closes.
	ended := time.Now()
	r.Status, r.ExitCode, r.EndedAt = run.StatusFailed, ptr(2), &ended
	if err := c.Save(ctx, r); err != nil {
		t.Logf("Save() reported %v, which is allowed as long as the tail still lands", err)
	}

	stored, err := backing.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !stored.Status.Terminal() {
		t.Fatalf("the run did not finish: %q", stored.Status)
	}

	log, err := backing.Log(ctx, r.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if !strings.Contains(string(log), recap) {
		if stored.Warning == "" {
			t.Errorf("the stored log is missing its last section and the run says nothing about it, so "+
				"the outcome commits a digest over a truncated log and the receipt attests it as "+
				"complete:\nlog: %q", log)
		} else {
			t.Logf("the tail could not be delivered, and the run records it: %s", stored.Warning)
		}
	}
}

// TestATailThatNeverLandsIsDeclared is the other side. An outage that outlasts the wait cannot hold the
// run's status hostage, so the record closes anyway. What it must not do is close silently: the outcome
// digests the log that arrived, so a log missing its end has to say it is missing its end, or the receipt
// attests a truncated log as the whole of it.
func TestATailThatNeverLandsIsDeclared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()

	// A control node whose log endpoint never recovers.
	inner := relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	claimed := time.Now()
	r := &run.Run{
		ID: "run_lost_tail", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	if err := c.AppendLog(ctx, r.ID, []byte("PLAY RECAP *** failed=1\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	ended := time.Now()
	r.Status, r.ExitCode, r.EndedAt = run.StatusFailed, ptr(2), &ended
	_ = c.Save(ctx, r)

	// The run still reaches its end: an undeliverable log must not leave the status unknown.
	stored, err := backing.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !stored.Status.Terminal() {
		t.Fatalf("an undeliverable tail left the run %q, so its status is never reported", stored.Status)
	}
	// And it says the log is short.
	if stored.Warning == "" {
		t.Error("the run's log is missing its end and the record says nothing about it, so the outcome " +
			"digest attests a truncated log as complete")
	}
}

// ptr returns a pointer to v, for the exit code a finished run carries.
func ptr[T any](v T) *T { return &v }
