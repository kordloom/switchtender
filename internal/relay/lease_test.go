package relay_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// leaseHeader is the header the claim response carries the per-claim capability in and every report
// presents it back through. It mirrors the unexported constant in the server, named here so the
// tests craft requests the way a worker does.
const leaseHeader = "X-Switchtender-Lease"

// postWithLease posts body to the relay path with the shared worker token and, when lease is
// non-empty, the per-claim capability. It returns the status so a test can assert what the server
// admits. Passing an empty lease is how a forger who never claimed the run reports on it.
func postWithLease(t *testing.T, base, path, lease string, body []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path,
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testWorkerToken)
	if lease != "" {
		req.Header.Set(leaseHeader, lease)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestForgedReportRefusedWithoutTheClaimSecret is the vulnerability this capability closes. Every
// worker presents the same shared token, and the relay's run read discloses the lease name, so a
// worker that never held a run could name its holder and forge a terminal report for it. The report
// check replayed that disclosed name as the only identity, so the forgery was admitted. With a
// per-claim secret the name is no longer proof: only the worker the claim was granted to holds the
// capability, and a report that does not present it is refused whatever name it carries.
func TestForgedReportRefusedWithoutTheClaimSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	// A run worker-a holds, with the capability its claim minted. Seeding it directly is the same
	// state Claim leaves behind, and lets the test hold the secret worker-b must not be able to guess.
	held := &run.Run{
		ID: "run_a", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, ClaimSecret: "secret-a",
	}
	if err := backing.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// worker-b replays worker-a's disclosed lease name to forge a terminal report. It never claimed
	// the run, so it does not hold the capability.
	forge := []byte(`{"id":"run_a","status":"failed","claimed_by":"worker-a"}`)
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_a/save", "", forge); code < 400 {
		t.Errorf("a report with no lease terminalized worker-a's run: HTTP %d", code)
	}
	// Even guessing at the capability does not help: the compare is exact.
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_a/save", "secret-b", forge); code < 400 {
		t.Errorf("a report with a wrong lease terminalized worker-a's run: HTTP %d", code)
	}
	if got, err := backing.Get(ctx, "run_a"); err != nil {
		t.Fatalf("Get() error = %v", err)
	} else if got.Status != run.StatusRunning {
		t.Fatalf("run status = %q, want it untouched as running after the forgeries", got.Status)
	}

	// The holder, presenting the capability its claim minted, is admitted.
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_a/save", "secret-a", forge); code != http.StatusNoContent {
		t.Fatalf("the holder's report was refused: HTTP %d", code)
	}
	if got, err := backing.Get(ctx, "run_a"); err != nil {
		t.Fatalf("Get() error = %v", err)
	} else if got.Status != run.StatusFailed {
		t.Errorf("run status = %q, want failed once the holder reported", got.Status)
	}
}

// TestRecordWritersAndHeartbeatRequireTheClaimSecret checks the capability gates the writes that
// build the record and the heartbeat that renews the lease, not only the status report. What an
// approver reads while deciding is exactly the thing worth forging, so a worker that cannot present
// the capability may neither append to a run's output nor keep its lease alive.
func TestRecordWritersAndHeartbeatRequireTheClaimSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	held := &run.Run{
		ID: "run_rec", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, ClaimSecret: "secret-rec",
	}
	if err := backing.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	forged := []byte("PLAY RECAP ok=12 failed=0\n")
	// A record write with no capability is refused, and nothing lands in the run's output.
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_rec/log", "", forged); code < 400 {
		t.Errorf("a log write with no lease was admitted: HTTP %d", code)
	}
	if got, err := backing.Log(ctx, "run_rec"); err != nil {
		t.Fatalf("Log() error = %v", err)
	} else if bytes.Contains(got, forged) {
		t.Errorf("the run now records %q, which no holder wrote", got)
	}
	// The events writer draws the same boundary.
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_rec/events", "", []byte("[]")); code < 400 {
		t.Errorf("an events write with no lease was admitted: HTTP %d", code)
	}

	// The holder's record write is admitted.
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_rec/log", "secret-rec",
		[]byte("ok: [web01]\n")); code != http.StatusNoContent {
		t.Fatalf("the holder's log write was refused: HTTP %d", code)
	}
	if got, err := backing.Log(ctx, "run_rec"); err != nil {
		t.Fatalf("Log() error = %v", err)
	} else if !bytes.Contains(got, []byte("ok: [web01]")) {
		t.Errorf("output = %q, want the holder's line recorded", got)
	}

	// The heartbeat renews the lease and answers the same question. Without the capability it is
	// told the run is not found, the same non-leaking answer a run in another pool gets.
	hb := []byte(`{"id":"run_rec","owner":"worker-a"}`)
	if code := postWithLease(t, ts.URL, "/relay/v1/heartbeat", "", hb); code != http.StatusNotFound {
		t.Errorf("a heartbeat with no lease answered %d, want 404", code)
	}
	if code := postWithLease(t, ts.URL, "/relay/v1/heartbeat", "secret-rec", hb); code != http.StatusNoContent {
		t.Errorf("the holder's heartbeat answered %d, want 204", code)
	}
}

// TestRunClaimedBeforeTheCapabilityStillChecksTheHolder proves the upgrade does not strand runs
// already in flight, and does not weaken the boundary for them either. A run claimed before the
// capability existed carries no secret. Its holder, which cannot present a capability that was never
// minted, is still admitted on the older lease-name check, and an impostor naming its own lease is
// still refused. This is the self-healing migration: existing runs keep working, and every run
// claimed after the upgrade is protected by the secret.
func TestRunClaimedBeforeTheCapabilityStillChecksTheHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	// No ClaimSecret: the state an in-flight run has at the moment the control node upgrades.
	legacy := &run.Run{
		ID: "run_legacy", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed,
	}
	if err := backing.Save(ctx, legacy); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// An impostor naming its own lease is refused: the holder check still applies when there is no
	// secret to prove identity with.
	impostor := []byte(`{"id":"run_legacy","status":"failed","claimed_by":"worker-b"}`)
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_legacy/save", "", impostor); code < 400 {
		t.Errorf("an impostor terminalized a legacy run: HTTP %d", code)
	}
	if got, err := backing.Get(ctx, "run_legacy"); err != nil {
		t.Fatalf("Get() error = %v", err)
	} else if got.Status != run.StatusRunning {
		t.Fatalf("legacy run status = %q, want it untouched as running", got.Status)
	}

	// The real holder, which has no capability to present because none was minted, is admitted on
	// the lease-name check, so the run is not stranded by the upgrade.
	holder := []byte(`{"id":"run_legacy","status":"succeeded","claimed_by":"worker-a"}`)
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_legacy/save", "", holder); code != http.StatusNoContent {
		t.Fatalf("the holder of a legacy run was refused: HTTP %d", code)
	}
	if got, err := backing.Get(ctx, "run_legacy"); err != nil {
		t.Fatalf("Get() error = %v", err)
	} else if got.Status != run.StatusSucceeded {
		t.Errorf("legacy run status = %q, want succeeded once its holder reported", got.Status)
	}
}
