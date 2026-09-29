package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// errRefusedTerminalWrite is the error a store returns for a terminal write it will never accept. A
// single 0x00 byte in a run's failure text made real PostgreSQL reject the write this way on every
// retry, which is what turned a transient-looking failure into a permanent one.
var errRefusedTerminalWrite = errors.New("save run: invalid byte sequence for encoding UTF8: 0x00")

// failTerminalStore is a run.Store whose terminal write always fails, however many times it is
// retried. Every other operation is served by the wrapped store, so a run reaches the finalize path
// normally and only the write that records how it ended is refused.
type failTerminalStore struct {
	run.Store
	// refused counts terminal writes the store turned down, so a test can prove it was asked.
	refused atomic.Int64
}

// Save refuses to record a terminal run and serves every other save from the wrapped store.
func (s *failTerminalStore) Save(ctx context.Context, r *run.Run) error {
	if r.Status.Terminal() {
		s.refused.Add(1)
		return errRefusedTerminalWrite
	}
	return s.Store.Save(ctx, r)
}

// FinalizeRunning refuses the atomic terminal write the same way, so the run stays running.
func (s *failTerminalStore) FinalizeRunning(context.Context, string, run.Finalization) (bool, error) {
	s.refused.Add(1)
	return false, errRefusedTerminalWrite
}

// TestFinalizeCommitsNoOutcomeWhenTerminalWriteFails proves that a run whose terminal write the
// store will not accept is left running for the janitor and puts nothing on the chain.
//
// Recording the outcome on the tamper-evident chain while the database never learned the run
// finished is the worst of both: the chain asserts an outcome whose digest can never be recomputed
// from the stored evidence, so the run's own receipt reports the chain and the database disagree,
// and the run sits terminal with no exit code where no sweep will ever reclaim it.
func TestFinalizeCommitsNoOutcomeWhenTerminalWriteFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := run.NewMemStore()
	store := &failTerminalStore{Store: mem}
	audits := audit.NewMemStore()
	d := New(store, okRunner(), nil, WithAudits(audits))

	started := time.Now()
	r := &run.Run{
		ID: "run_fail", Playbook: "site.yml", Inventory: "inv", Status: run.StatusRunning,
		CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-1", ClaimedAt: &started,
		Actor: "alice",
	}
	if err := mem.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	code := 0
	d.finalize(r, run.StatusSucceeded, &code, "")

	if store.refused.Load() == 0 {
		t.Fatal("the store was never asked to record the terminal state")
	}

	got, err := mem.Get(ctx, "run_fail")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("stored status = %q, want running: a terminal write that never landed must not "+
			"move the run out of the janitor's reach", got.Status)
	}
	if got.ExitCode != nil {
		t.Errorf("stored exit code = %d, want none recorded", *got.ExitCode)
	}
	if got.EndedAt != nil {
		t.Errorf("stored ended_at = %v, want none recorded", got.EndedAt)
	}

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if e.Method == audit.MethodRun && strings.Contains(e.Path, "run_fail") {
			body, berr := outcome.Body(ctx, mem, got)
			if berr != nil {
				t.Fatalf("Body() error = %v", berr)
			}
			t.Errorf("an outcome entry was committed for a run the store never recorded: path %q, "+
				"digest recomputes from the stored run = %v", e.Path,
				audit.VerifyContentDigest(e.ContentDigest, e.Nonce, body))
		}
	}

	// The janitor owns the run now, and its sweep is what turns an executor that stopped without
	// recording a result into a retryable interrupted run.
	if _, err := mem.ReclaimStale(ctx, 0); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	swept, err := mem.Get(ctx, "run_fail")
	if err != nil {
		t.Fatalf("Get() after sweep error = %v", err)
	}
	if swept.Status != run.StatusInterrupted {
		t.Errorf("status after the sweep = %q, want interrupted", swept.Status)
	}
}

// TestFinalizeStoresWhatTheOutcomeDigestCommits is the other half of the guarantee: when the
// terminal write does land, everything the committed digest is taken over is in the stored run, so
// an auditor holding the receipt can rebuild the body and match it. The container image is the
// field this turns on. An executor only resolves it while the run is under way, from the project or
// server default, so it reaches the store with the terminal record or not at all.
func TestFinalizeStoresWhatTheOutcomeDigestCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	d := New(store, okRunner(), nil, WithAudits(audits))

	started := time.Now()
	r := &run.Run{
		ID: "run_img", Playbook: "site.yml", Inventory: "inv", Status: run.StatusRunning,
		CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-1", ClaimedAt: &started,
		Actor: "alice",
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// The default image resolved while the run executed, exactly as streamSpec records it.
	r.Image = "ghcr.io/example/runner:1.2"

	code := 0
	d.finalize(r, run.StatusSucceeded, &code, "")

	got, err := store.Get(ctx, "run_img")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("stored status = %q, want succeeded", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("stored exit code = %v, want 0", got.ExitCode)
	}
	if got.EndedAt == nil {
		t.Error("stored ended_at = nil, want the end time recorded")
	}
	if got.Image != "ghcr.io/example/runner:1.2" {
		t.Errorf("stored image = %q, want the image the run executed in", got.Image)
	}

	entry := waitOutcomeEntry(t, audits, "run_img")
	body, err := outcome.Body(ctx, store, got)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	if !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, body) {
		t.Error("the committed outcome digest does not recompute from the stored run")
	}
}
