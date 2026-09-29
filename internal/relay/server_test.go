package relay_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	ts := httptest.NewServer(relay.NewHandler(backing, testWorkerToken, nil))
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
	if err := c.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
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
	ts := httptest.NewServer(relay.NewHandler(run.NewMemStore(), testWorkerToken, nil))
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
