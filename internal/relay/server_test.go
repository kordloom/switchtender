package relay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// testWorkerToken is the bearer token the relay server and client share in these tests.
const testWorkerToken = "worker-secret"

// newRelay stands up a relay server over a fresh memStore and a Client that dials it over the HTTP
// transport, returning the Client and the backing store so a test can confirm writes landed.
func newRelay(t *testing.T) (*relay.Client, run.Store) {
	t.Helper()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))
	return c, backing
}

// TestServerRoundTrip proves a run flows end to end through the relay Client, the HTTP transport, and
// the relay server into the backing store: a saved run claims, heartbeats, and records output,
// events, and a host summary, and every write lands in the backing memStore as if the worker held it
// directly.
func TestServerRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, backing := newRelay(t)

	r := &run.Run{ID: "run_mesh", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now()}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	claimed, err := c.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.ID != "run_mesh" {
		t.Fatalf("claimed %q, want run_mesh", claimed.ID)
	}
	if err := c.Heartbeat(ctx, "run_mesh", "worker-a"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if err := c.AppendLog(ctx, "run_mesh", []byte("hello\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	events := []event.Event{{Type: event.TypeRunnerOK, Host: "web01", Task: "ping"}}
	if err := c.AppendEvents(ctx, "run_mesh", events); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := c.SaveHostSummary(ctx, "run_mesh", []run.HostSummary{{Host: "web01", Changed: 1}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}

	got, err := c.Get(ctx, "run_mesh")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ClaimedBy != "worker-a" {
		t.Errorf("claimed_by = %q, want worker-a", got.ClaimedBy)
	}

	// Output is coalesced, so finishing the run is what flushes the last of it.
	got.Status = run.StatusSucceeded
	if err := c.Save(ctx, got); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	// The writes reached the backing store through the relay.
	gotLog, err := backing.Log(ctx, "run_mesh")
	if err != nil {
		t.Fatalf("backing Log() error = %v", err)
	}
	if diff := cmp.Diff("hello\n", string(gotLog)); diff != "" {
		t.Errorf("backing log mismatch (-want +got):\n%s", diff)
	}
	gotEvents, err := backing.Events(ctx, "run_mesh")
	if err != nil {
		t.Fatalf("backing Events() error = %v", err)
	}
	if diff := cmp.Diff(events, gotEvents, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("backing events mismatch (-want +got):\n%s", diff)
	}
	gotSummaries, err := backing.HostHistory(ctx, "web01", 10)
	if err != nil {
		t.Fatalf("backing HostHistory() error = %v", err)
	}
	want := []run.HostSummary{{RunID: "run_mesh", Host: "web01", Changed: 1}}
	if diff := cmp.Diff(want, gotSummaries, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("backing host summaries mismatch (-want +got):\n%s", diff)
	}
}

// countingRelay stands up a relay server that counts the log posts reaching it, returning the Client
// and a func that reads the count so a test can measure requests rather than assert about them.
func countingRelay(t *testing.T) (*relay.Client, run.Store, func() int) {
	t.Helper()
	backing := run.NewMemStore()
	var mu sync.Mutex
	posts := 0
	inner := relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			mu.Lock()
			posts++
			mu.Unlock()
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))
	return c, backing, func() int {
		mu.Lock()
		defer mu.Unlock()
		return posts
	}
}

// TestAppendLogCoalesces proves many small writes cost far fewer requests than writes, and that
// finishing the run flushes every byte. Without coalescing each write is its own POST, which is what
// made a chatty run on a remote worker a request per chunk.
func TestAppendLogCoalesces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, backing, posts := countingRelay(t)

	r := &run.Run{ID: "run_chatty", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(), ClaimedBy: "worker-a"}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	const writes = 500
	var want strings.Builder
	for i := range writes {
		line := fmt.Sprintf("line %d\n", i)
		want.WriteString(line)
		if err := c.AppendLog(ctx, "run_chatty", []byte(line)); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
	}
	r.Status = run.StatusSucceeded
	if err := c.Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	// 500 writes must not cost 500 requests. The bound is generous so the test measures coalescing
	// rather than the exact timing of the delay flush.
	if got := posts(); got >= writes/10 {
		t.Errorf("log posts = %d for %d writes, want far fewer", got, writes)
	}
	t.Logf("%d writes cost %d log posts", writes, posts())

	// Nothing was stranded by the buffering.
	gotLog, err := backing.Log(ctx, "run_chatty")
	if err != nil {
		t.Fatalf("backing Log() error = %v", err)
	}
	if diff := cmp.Diff(want.String(), string(gotLog)); diff != "" {
		t.Errorf("backing log mismatch (-want +got):\n%s", diff)
	}
}

// TestAppendLogFlushesOnDelay proves a run that goes quiet still has its output posted without
// waiting for the run to finish, so a live tail is not held back by an unfilled batch.
func TestAppendLogFlushesOnDelay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, backing, posts := countingRelay(t)

	r := &run.Run{ID: "run_quiet", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(), ClaimedBy: "worker-a"}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	if err := c.AppendLog(ctx, "run_quiet", []byte("one line\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if got := posts(); got != 0 {
		t.Fatalf("log posts = %d immediately after a small write, want 0", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for posts() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	gotLog, err := backing.Log(ctx, "run_quiet")
	if err != nil {
		t.Fatalf("backing Log() error = %v", err)
	}
	if diff := cmp.Diff("one line\n", string(gotLog)); diff != "" {
		t.Errorf("delayed flush mismatch (-want +got):\n%s", diff)
	}
}

// TestAppendLogFlushesOnSize proves a burst larger than the batch size posts without waiting out the
// delay, so a loud run does not buffer without bound between flushes.
func TestAppendLogFlushesOnSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, backing, posts := countingRelay(t)

	r := &run.Run{ID: "run_loud", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(), ClaimedBy: "worker-a"}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	big := bytes.Repeat([]byte("x"), 128<<10)
	if err := c.AppendLog(ctx, "run_loud", big); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if got := posts(); got != 1 {
		t.Errorf("log posts = %d after an oversized write, want 1 without waiting", got)
	}
	gotLog, err := backing.Log(ctx, "run_loud")
	if err != nil {
		t.Fatalf("backing Log() error = %v", err)
	}
	if len(gotLog) != len(big) {
		t.Errorf("backing log = %d bytes, want %d", len(gotLog), len(big))
	}
}

// TestAppendLogOrderUnderConcurrency proves coalesced output keeps the order it was written in when
// the size and delay flushes overlap, since a scrambled log is worse than a chatty one.
func TestAppendLogOrderUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, backing, _ := countingRelay(t)

	r := &run.Run{ID: "run_order", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(), ClaimedBy: "worker-a"}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	var want strings.Builder
	for i := range 200 {
		line := fmt.Sprintf("%04d\n", i)
		want.WriteString(line)
		if err := c.AppendLog(ctx, "run_order", []byte(line)); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
		if i%50 == 0 {
			time.Sleep(2 * relay.LogBatchDelayForTest())
		}
	}
	r.Status = run.StatusSucceeded
	if err := c.Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	gotLog, err := backing.Log(ctx, "run_order")
	if err != nil {
		t.Fatalf("backing Log() error = %v", err)
	}
	if diff := cmp.Diff(want.String(), string(gotLog)); diff != "" {
		t.Errorf("log order mismatch (-want +got):\n%s", diff)
	}
}

// TestServerClaimNonePending confirms a claim against an empty store maps the server's 204 back to
// run.ErrNonePending, so the claim loop treats it as an idle poll rather than a failure.
func TestServerClaimNonePending(t *testing.T) {
	t.Parallel()
	c, _ := newRelay(t)
	if _, err := c.Claim(context.Background(), "worker-a", []string{""}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("Claim() error = %v, want ErrNonePending", err)
	}
}

// TestServerHeartbeatNotFound confirms a heartbeat for a run the store does not hold maps the
// server's 404 back to run.ErrNotFound.
func TestServerHeartbeatNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newRelay(t)
	if err := c.Heartbeat(context.Background(), "run_gone", "worker-a"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Heartbeat() error = %v, want ErrNotFound", err)
	}
}

// TestServerUnauthorized confirms the relay endpoints reject a request that presents no token or the
// wrong token with a 401, so the worker path is never open.
func TestServerUnauthorized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Present string
	}{
		{Name: "missing", Present: ""},
		{Name: "wrong", Present: "Bearer nope"},
		{Name: "empty-bearer", Present: "Bearer "},
	}
	ts := httptest.NewServer(relay.NewHandler(run.NewMemStore(), relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/relay/v1/claim",
				strings.NewReader(`{"owner":"worker-a","queues":[""]}`))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if test.Present != "" {
				req.Header.Set("Authorization", test.Present)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}
		})
	}
}
