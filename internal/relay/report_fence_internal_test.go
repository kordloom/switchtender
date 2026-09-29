package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAWorkerReportNeverCarriesTheLeaseOrTheSpec pins the split applyWorkerReport documents as the
// security boundary: a worker reports what it learned by executing, and nothing that decides what
// executes. Copying the holder in from the wire is what turned one request into a remote kill switch,
// because a running parent with no holder is what the abandoned-parent sweep settles.
func TestAWorkerReportNeverCarriesTheLeaseOrTheSpec(t *testing.T) {
	t.Parallel()
	created := time.Now().Add(-time.Hour)
	started := created.Add(time.Minute)
	stored := &run.Run{
		ID: "r1", Playbook: "site.yml", Inventory: "prod.ini", Queue: "production",
		Status: run.StatusRunning, CreatedAt: created, ClaimedBy: "worker-a",
		Image: "ghcr.io/org/runner@sha256:pinned", PullCredentialID: "cred-control-node",
		CredentialIDs: []string{"cred-prod"}, ProjectID: "proj-1", Limit: "web",
	}
	reported := &run.Run{
		ID: "r1", Playbook: "attacker.yml", Inventory: "dmz.ini", Queue: "dmz",
		Status: run.StatusRunning, StartedAt: &started, ClaimedBy: "",
		Image: "docker.io/attacker/runner:latest", PullCredentialID: "cred-attacker",
		CredentialIDs: []string{"cred-attacker"}, ProjectID: "proj-attacker", Limit: "all",
		CommitSHA: "abc123",
	}

	applyWorkerReport(stored, reported)

	if stored.ClaimedBy != "worker-a" {
		t.Errorf("ClaimedBy = %q after a worker report, want the holder the control node granted: "+
			"taking the lease from the wire lets one request clear a live run's holder, which is "+
			"exactly what the abandoned-parent sweep settles", stored.ClaimedBy)
	}
	unchanged := map[string][2]string{
		"Playbook":         {"site.yml", stored.Playbook},
		"Inventory":        {"prod.ini", stored.Inventory},
		"Queue":            {"production", stored.Queue},
		"Image":            {"ghcr.io/org/runner@sha256:pinned", stored.Image},
		"PullCredentialID": {"cred-control-node", stored.PullCredentialID},
		"ProjectID":        {"proj-1", stored.ProjectID},
		"Limit":            {"web", stored.Limit},
	}
	for field, pair := range unchanged {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q after a worker report, want %q: a worker that could change what "+
				"executes could change what it was authorized to do", field, pair[1], pair[0])
		}
	}
	if diff := cmp.Diff([]string{"cred-prod"}, stored.CredentialIDs); diff != "" {
		t.Errorf("CredentialIDs mismatch (-want +got):\n%s", diff)
	}
	// The fields a worker does learn by running still arrive.
	if stored.CommitSHA != "abc123" {
		t.Errorf("CommitSHA = %q, want the value the executor reported", stored.CommitSHA)
	}
	if stored.StartedAt == nil || !stored.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want the executor's %v", stored.StartedAt, started)
	}
}

// TestATerminalReportCannotRestampTheImageOrItsCredential pins the same boundary at the one place the
// worker's decoded body reaches the store on the terminal path.
//
// The finalization is built field by field, and every field naming what ran has to come from the run
// the control node holds. Image and the credential that pulls it decide which code executes and with
// which registry identity, so sourcing either from the report would let a worker attest that a run
// executed an image the control node never chose, in the row its outcome entry and receipt digest.
func TestATerminalReportCannotRestampTheImageOrItsCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	created := time.Now().Add(-time.Hour)
	seeded := &run.Run{
		ID: "r_image", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: created,
		ClaimedBy: "worker-a", ClaimSecret: "the-capability", Image: "ghcr.io/org/runner@sha256:pinned",
		PullCredentialID: "cred-control-node",
	}
	if err := backing.Save(ctx, seeded); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	srv := httptest.NewServer(NewHandler(backing, SinglePool("tok"), nil, nil, nil))
	t.Cleanup(srv.Close)

	started := created.Add(time.Minute)
	ended := created.Add(2 * time.Minute)
	body, err := json.Marshal(&run.Run{
		ID: "r_image", Status: run.StatusSucceeded, ClaimedBy: "worker-a",
		StartedAt: &started, EndedAt: &ended,
		Image: "docker.io/attacker/runner:latest", PullCredentialID: "cred-attacker",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/relay/v1/runs/r_image/save", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set(leaseHeader, "the-capability")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("save request error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("save status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	final, err := backing.Get(ctx, "r_image")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q, want the outcome the worker reported", final.Status)
	}
	if final.Image != "ghcr.io/org/runner@sha256:pinned" {
		t.Errorf("image = %q, want the image the control node pinned: a worker that can restamp it "+
			"attests an execution of code the control node never chose", final.Image)
	}
	if final.PullCredentialID != "cred-control-node" {
		t.Errorf("pull credential = %q, want the one the control node chose", final.PullCredentialID)
	}
}

// fenceOwnerStore is a store whose second and later reads of a run report a different holder, standing
// in for a janitor sweep and a re-claim landing between the guard read and the fenced write. It records
// the owner the fenced progress write was handed.
type fenceOwnerStore struct {
	// Store serves everything the test does not stage.
	run.Store
	// SummaryAppender serves the continuation writes, which this test does not make.
	run.SummaryAppender
	// mu guards gets and owner.
	mu sync.Mutex
	// gets counts how many times a run has been read.
	gets int
	// owner is the holder the fenced progress write was given.
	owner string
}

// Get reports the seeded holder on the first read and a different one afterwards, which is the
// divergence the fence exists to survive.
func (f *fenceOwnerStore) Get(ctx context.Context, id string) (*run.Run, error) {
	f.mu.Lock()
	f.gets++
	n := f.gets
	f.mu.Unlock()
	got, err := f.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if n > 1 {
		got.ClaimedBy = "worker-b"
	}
	return got, nil
}

// ApplyRunningProgress records the owner the handler fenced the write with.
func (f *fenceOwnerStore) ApplyRunningProgress(_ context.Context, _, owner string,
	_ run.Progress) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner = owner
	return true, nil
}

// TestProgressIsFencedWithTheRowItReReadNotTheSnapshot pins which holder the running-progress write is
// fenced with.
//
// The handler reads the run twice: once for the guards, and again for the write. Between them a sweep
// can settle or requeue the run and another executor can take it. Fencing with the first read defeats
// the point of taking the second, because the write then carries a holder the store no longer agrees
// with and the stale report lands anyway. The value has to come from the row the fence was read
// against, never from the snapshot the guards ran on and never from the body the worker sent.
func TestProgressIsFencedWithTheRowItReReadNotTheSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := run.NewMemStore()
	created := time.Now().Add(-time.Hour)
	if err := mem.Save(ctx, &run.Run{
		ID: "r_fence", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: created,
		ClaimedBy: "worker-a", ClaimSecret: "the-capability",
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	appender, ok := mem.(run.SummaryAppender)
	if !ok {
		t.Fatal("the memory store is not a run.SummaryAppender")
	}
	staged := &fenceOwnerStore{Store: mem, SummaryAppender: appender}
	srv := httptest.NewServer(NewHandler(staged, SinglePool("tok"), nil, nil, nil))
	t.Cleanup(srv.Close)

	started := created.Add(time.Minute)
	body, err := json.Marshal(&run.Run{
		ID: "r_fence", Status: run.StatusRunning, ClaimedBy: "worker-a", StartedAt: &started,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/relay/v1/runs/r_fence/save", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set(leaseHeader, "the-capability")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("save request error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("save status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	staged.mu.Lock()
	defer staged.mu.Unlock()
	if staged.owner != "worker-b" {
		t.Errorf("progress was fenced with owner %q, want %q: the fence has to carry the holder from "+
			"the row it re-read, not the snapshot the guards ran against nor the name the worker sent",
			staged.owner, "worker-b")
	}
}

// TestATerminalSaveKeepsATailItCouldNotDeliver pins that the batch a terminal save cannot flush stays
// held rather than being dropped with the run.
//
// Save drops a finished run's batch, which is right when there is nothing left in it. Dropping it
// unconditionally throws away the end of the run's output over a blip the relay recovers from a moment
// later, and the control node has already committed the run's outcome over the log it did receive, so
// the truncated log is attested as the complete one. The batch is released by the delivery that lands
// it or by the abandon window, never by the save that could not send it.
func TestATerminalSaveKeepsATailItCouldNotDeliver(t *testing.T) {
	t.Parallel()
	tr, flaky, _ := newFlakyTransport(t, "run_terminal_tail")
	const tail = "the last line of the run\n"

	flaky.down.Store(true)
	if err := tr.AppendLog(context.Background(), "run_terminal_tail", []byte(tail)); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	// The terminal save runs against a relay that is not answering. The context is already done, so the
	// tail window returns at once instead of spending its full length on a fault the test has staged.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ended := time.Now()
	_ = tr.Save(ctx, &run.Run{
		ID: "run_terminal_tail", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Now().Add(-time.Minute), EndedAt: &ended, ClaimedBy: "worker-a",
	})

	if got := bufferedBytes(t, tr, "run_terminal_tail"); got != len(tail) {
		t.Fatalf("buffered = %d bytes after a terminal save that could not flush, want the %d it "+
			"still holds: dropping them loses the end of the run's output while the control node "+
			"commits an outcome that digests the log as complete", got, len(tail))
	}

	// Once the relay answers, the tail it kept is delivered.
	flaky.down.Store(false)
	if err := tr.flushLog(context.Background(), "run_terminal_tail"); err != nil {
		t.Fatalf("flushLog() error = %v once the relay returned", err)
	}
	if got := bufferedBytes(t, tr, "run_terminal_tail"); got != 0 {
		t.Errorf("buffered = %d bytes after the tail landed, want 0", got)
	}
}
