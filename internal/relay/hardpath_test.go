package relay_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAStoreFaultOnAReportIsAFiveHundredNotASilentSuccess pins that a write the control node could not
// perform is reported as a failure, so the worker retries rather than moving on.
//
// Each of these is the last chance the evidence has to reach the store. A run's per-host outcomes, its
// task timings, and its event stream are what the committed outcome and the receipt are built from, so
// a write that failed and answered 204 would produce a run whose record is quietly missing the part
// that says what happened on the hosts.
func TestAStoreFaultOnAReportIsAFiveHundredNotASilentSuccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Break func(*faultyStore)
		Name  string
		Path  string
		Body  string
	}{{ // Test 0: The whole-report host write.
		Name: "host summary", Path: "/relay/v1/runs/run_fault/host-summary",
		Body: `[{"host":"web01"}]`, Break: func(f *faultyStore) { f.failSaveHostSummary = true },
	}, { // Test 1: The continuation host write, which takes the other branch entirely.
		Name: "host summary continuation",
		Path: "/relay/v1/runs/run_fault/host-summary?part=continue",
		Body: `[{"host":"web01"}]`, Break: func(f *faultyStore) { f.failAppendHostSummary = true },
	}, { // Test 2: The whole-report task write.
		Name: "task summary", Path: "/relay/v1/runs/run_fault/task-summary",
		Body: `[{"task":"ping"}]`, Break: func(f *faultyStore) { f.failSaveTaskSummary = true },
	}, { // Test 3: The continuation task write.
		Name: "task summary continuation",
		Path: "/relay/v1/runs/run_fault/task-summary?part=continue",
		Body: `[{"task":"ping"}]`, Break: func(f *faultyStore) { f.failAppendTaskSummary = true },
	}, { // Test 4: The event write.
		Name: "events", Path: "/relay/v1/runs/run_fault/events",
		Body:  `[{"type":"runner_ok","host":"web01"}]`,
		Break: func(f *faultyStore) { f.failAppendEvents = true },
	}, { // Test 5: The facts write, reached only after the host scope check has passed.
		Name: "host facts", Path: "/relay/v1/runs/run_fault/host-facts",
		Body:  `[{"host":"web01","facts":{"os":"linux"}}]`,
		Break: func(f *faultyStore) { f.failSaveHostFacts = true },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := newFaultyStore(t)
			fixture := newRelayFixture(t, store, nil)
			seedClaimed(t, fixture.Store, "run_fault")
			// The facts write is scoped to hosts the run recorded, so that result is stored first.
			if err := store.Store.SaveHostSummary(context.Background(), "run_fault",
				[]run.HostSummary{{Host: "web01"}}); err != nil {
				t.Fatalf("seed SaveHostSummary() error = %v", err)
			}
			test.Break(store)

			status, body := post(t, fixture.URL, http.MethodPost, test.Path, bearer, test.Body)
			if status == http.StatusNoContent {
				t.Fatalf("%s answered 204 while the store could not perform the write, so the "+
					"worker moves on and the evidence is silently lost", test.Name)
			}
			if status != http.StatusInternalServerError {
				t.Errorf("%s answered %d (%s), want 500", test.Name, status, body)
			}
			if strings.Contains(body, errFaulty.Error()) {
				t.Errorf("%s put the store's own error text on the wire: %s", test.Name, body)
			}
		})
	}
}

// TestAFencedWriteFaultIsAFiveHundredNotAConflict pins that a store that could not perform the fenced
// status write is answered as a fault, not as the run having settled elsewhere.
//
// The two answers tell a worker opposite things. A conflict says the run is finished and there is
// nothing more to report, so the worker stops and the outcome it holds is discarded. A fault says the
// control node failed, so the worker retries and the outcome still lands. Reading one as the other
// loses the result of work that already happened on real hosts.
func TestAFencedWriteFaultIsAFiveHundredNotAConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Break func(*faultyStore)
		Name  string
		Body  string
	}{{ // Test 0: The terminal transition.
		Name: "terminal", Body: `{"status":"succeeded","claimed_by":"worker-a"}`,
		Break: func(f *faultyStore) { f.failFinalize = true },
	}, { // Test 1: The running progress report.
		Name: "running", Body: `{"status":"running","claimed_by":"worker-a"}`,
		Break: func(f *faultyStore) { f.failProgress = true },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := newFaultyStore(t)
			fixture := newRelayFixture(t, store, nil)
			seedClaimed(t, fixture.Store, "run_fenced")
			test.Break(store)

			status, body := post(t, fixture.URL, http.MethodPost,
				"/relay/v1/runs/run_fenced/save", bearer, test.Body)
			if status == http.StatusConflict {
				t.Fatalf("a %s save answered 409 on a store fault, so a worker throws away the "+
					"outcome of work that already happened", test.Name)
			}
			if status != http.StatusInternalServerError {
				t.Errorf("a %s save answered %d (%s), want 500", test.Name, status, body)
			}
			if strings.Contains(body, errFaulty.Error()) {
				t.Errorf("the response put the store's own error text on the wire: %s", body)
			}
		})
	}
}

// TestATruncatedLogPostIsNotHalfRecorded pins that output cut off in flight is not written as though
// it were complete.
//
// This is the ordinary shape of the failure the relay exists to survive: the link drops mid-request.
// The control node then holds a body it never finished reading, and appending it would write a
// truncated tail into the run's captured output and digest it as the run's log. Refusing the whole
// post is what lets the worker's own retry deliver the bytes intact instead.
func TestATruncatedLogPostIsNotHalfRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRelayFixture(t, nil, nil)
	seedClaimed(t, fixture.Store, "run_cut")

	host := strings.TrimPrefix(fixture.URL, "http://")
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	// A body is promised and only part of it is sent before the connection goes away.
	req := "POST /relay/v1/runs/run_cut/log HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Authorization: " + bearer + "\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Length: 4096\r\n\r\n" +
		strings.Repeat("x", 16)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// The handler has to have finished before the log is read, and it fails rather than blocking.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, gerr := fixture.Store.Log(ctx, "run_cut")
		if gerr != nil {
			t.Fatalf("Log() error = %v", gerr)
		}
		if len(got) != 0 {
			t.Fatalf("a truncated log post left %d bytes in the run's captured output, which is "+
				"then digested as the run's complete log", len(got))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAnUnencodableRunFailsTheCallRatherThanPostingAPartialBody pins that a run the transport cannot
// encode is refused before anything reaches the wire. A playbook publishes arbitrary values through
// set_stats, and those land on the run as its outputs, so a value JSON cannot represent is reachable
// from a playbook rather than only from a programming error. Half a body on the wire would be decoded
// by the control node as a report the worker never made.
func TestAnUnencodableRunFailsTheCallRatherThanPostingAPartialBody(t *testing.T) {
	t.Parallel()
	tr, script := scriptedTransport(t, http.StatusNoContent, "")
	bad := runningRun("run_unencodable")
	bad.Outputs = map[string]any{"handle": make(chan int)}

	err := tr.Save(context.Background(), bad)
	if err == nil {
		t.Fatal("Save() accepted a run whose outputs cannot be encoded")
	}
	if !strings.Contains(err.Error(), "encode request") {
		t.Errorf("Save() error = %q, want it to name the encode that failed", err)
	}
	if got := script.seen("/save"); got != 0 {
		t.Errorf("save requests = %d, want none: nothing may reach the wire when the body could "+
			"not be built", got)
	}
}

// TestARunTheControlNodeCannotSerializeIsAFaultNotATruncatedBody pins that a run which fails to
// marshal on the way out answers 500 with a JSON error rather than a 200 carrying half a run. A worker
// decoding a truncated run would either fail its own decode or, worse, act on the fields that made it
// through.
func TestARunTheControlNodeCannotSerializeIsAFaultNotATruncatedBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRelayFixture(t, nil, nil)
	r := seedClaimed(t, fixture.Store, "run_unserializable")
	r.Outputs = map[string]any{"handle": make(chan int)}
	if err := fixture.Store.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	stored, err := fixture.Store.Get(ctx, "run_unserializable")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, ok := stored.Outputs["handle"].(chan int); !ok {
		t.Skip("the store normalizes outputs, so an unserializable run cannot be stored")
	}

	status, body := post(t, fixture.URL, http.MethodGet, "/relay/v1/runs/run_unserializable",
		bearer, "")
	if status == http.StatusOK {
		t.Fatalf("a run the control node cannot serialize answered 200 with %q", body)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d (%s), want 500", status, body)
	}
}

// TestTheRelayHandlerSurvivesAnAbsentAuditStore pins that an install keeping no audit trail still
// serves the mesh. The record is written on the decisions that cross the boundary, and the append is
// deliberately not fail closed, so an install with no audit store must lease, execute, and finish runs
// exactly as one with a trail does rather than refusing a worker it cannot record.
func TestTheRelayHandlerSurvivesAnAbsentAuditStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	if err := backing.Save(ctx, &run.Run{ID: "run_untraced", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	// The nil audit store and the nil policy store are both the documented shape of an install that
	// configured neither.
	c, _ := clientOver(t, backing)

	claimed, err := c.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v with no audit store configured", err)
	}
	claimed.Status = run.StatusSucceeded
	if err := c.Save(ctx, claimed); err != nil {
		t.Fatalf("Save(succeeded) error = %v with no audit store configured", err)
	}
	got, err := backing.Get(ctx, "run_untraced")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("run status = %q, want succeeded: an install with no trail must still finish runs",
			got.Status)
	}
}

// clientOver stands up a relay server over the given store with no policy and no audit store, and
// returns a Client dialing it alongside the server's base URL.
func clientOver(t *testing.T, backing run.Store) (*relay.Client, string) {
	t.Helper()
	fixture := newRelayFixture(t, backing, nil)
	return relay.NewClient(relay.NewHTTPTransport(fixture.URL, testWorkerToken, nil)), fixture.URL
}
