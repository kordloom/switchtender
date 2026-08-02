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

// postAsWorker posts body to the relay path with the shared worker token and returns the status.
//
// The boundary is tested over HTTP rather than through the Client because the Client coalesces
// output: AppendLog answers before anything is sent, so a rejection is invisible from there. What
// the relay admits is a property of the server, and this asks the server directly.
func postAsWorker(t *testing.T, base, path string, body []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path,
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testWorkerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestWorkerTokenCannotWriteTheRecordOfARunItDoesNotHold checks that the holder boundary covers the
// record a person reads, not only the status a worker reports.
//
// Every worker presents the same token, so the token alone says nothing about which run a caller is
// executing. The lease is the only identity available. Without this the auxiliary writes took any
// run id: a worker refused a status report on a run awaiting a decision could still append a
// convincing "PLAY RECAP ok=12 failed=0" to that run's captured output, which is exactly what an
// approver reads while deciding whether to let that run proceed.
func TestWorkerTokenCannotWriteTheRecordOfARunItDoesNotHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	seed := []*run.Run{
		{ID: "run_held", Playbook: "site.yml", Status: run.StatusPendingApproval, CreatedAt: claimed},
		{ID: "run_unclaimed", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: claimed},
		{ID: "run_done", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: claimed,
			ClaimedBy: "worker-b", ClaimedAt: &claimed},
	}
	for _, r := range seed {
		if err := backing.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	forged := []byte("PLAY RECAP ok=12 failed=0\n")
	refusals := []struct {
		ID     string
		Reason string
	}{
		{"run_held", "an approver is still deciding on it"},
		{"run_unclaimed", "nobody has claimed it"},
		{"run_missing", "it does not exist"},
	}
	for _, c := range refusals {
		code := postAsWorker(t, ts.URL, "/relay/v1/runs/"+c.ID+"/log", forged)
		if code < 400 {
			t.Errorf("appending to %s answered %d, but %s", c.ID, code, c.Reason)
		}
		if c.ID == "run_missing" {
			continue
		}
		got, err := backing.Log(ctx, c.ID)
		if err != nil {
			t.Fatalf("Log(%s) error = %v", c.ID, err)
		}
		if bytes.Contains(got, forged) {
			t.Errorf("%s now records %q, which no execution produced", c.ID, got)
		}
	}

	// A finished run answers success and records nothing. The store drops the write either way, and
	// answering with a conflict only makes the transport retry a batch that can never land.
	if code := postAsWorker(t, ts.URL, "/relay/v1/runs/run_done/log", forged); code >= 400 {
		t.Errorf("appending to a finished run answered %d, which sends the transport into a "+
			"retry loop over output the store already drops", code)
	}
	if got, err := backing.Log(ctx, "run_done"); err != nil {
		t.Fatalf("Log(run_done) error = %v", err)
	} else if bytes.Contains(got, forged) {
		t.Errorf("a finished run now records %q", got)
	}

	// The other three writers build the same record and answer the same question.
	for _, path := range []string{"events", "host-summary", "task-summary"} {
		if code := postAsWorker(t, ts.URL, "/relay/v1/runs/run_held/"+path, []byte("[]")); code < 400 {
			t.Errorf("posting %s to a run awaiting a decision answered %d", path, code)
		}
	}
}

// TestWorkerReportBoundaryLeavesLegitimateWorkAlone checks the guard did not close the path a real
// worker uses. A leased, running run accepts output and both summaries.
func TestWorkerReportBoundaryLeavesLegitimateWorkAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	live := &run.Run{
		ID: "run_live", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed,
	}
	if err := backing.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if code := postAsWorker(t, ts.URL, "/relay/v1/runs/run_live/log",
		[]byte("ok: [web01]\n")); code != http.StatusNoContent {
		t.Fatalf("appending to a run this worker holds answered %d", code)
	}
	if code := postAsWorker(t, ts.URL, "/relay/v1/runs/run_live/host-summary",
		[]byte(`[{"host":"web01","ok":1}]`)); code != http.StatusNoContent {
		t.Fatalf("saving a host summary on a held run answered %d", code)
	}
	gotLog, err := backing.Log(ctx, live.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if !bytes.Contains(gotLog, []byte("ok: [web01]")) {
		t.Errorf("output = %q, want the worker's line recorded", gotLog)
	}
	summaries, err := backing.HostHistory(ctx, "web01", 10)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(summaries) != 1 {
		t.Errorf("host summaries = %d, want the worker's summary recorded", len(summaries))
	}
}
