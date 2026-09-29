package outcome_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestOutcomeDigestSurvivesTheStoreRoundTrip proves the outcome a run commits still verifies after
// the run has been through the database, on an install whose clock is not UTC.
//
// The digest is taken over the record built from the run in memory, where its timestamps carry the
// server's local offset. The store writes every timestamp as UTC and reads it back that way, so a
// receipt, which rebuilds the record from the stored run, produced different bytes and the flagship
// artifact reported "outcome FAILED" on every install outside UTC. Every existing test built its
// timestamps in UTC, so the whole suite agreed with a claim that only held in one time zone.
func TestOutcomeDigestSurvivesTheStoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	// A server west of Greenwich, which is where this defect lives.
	zone := time.FixedZone("CST", -6*60*60)
	started := time.Date(2026, 8, 17, 9, 30, 15, 123456789, zone)
	ended := started.Add(90 * time.Second)
	r := &run.Run{
		ID: "run_tz", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "operator-jane", ActorType: "session", CreatedAt: started,
		StartedAt: &started,
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	if err := store.Runs().SaveHostSummary(ctx, r.ID,
		[]run.HostSummary{{Host: "web01", OK: 3, Changed: 1, Worst: "changed", RanAt: started}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	r.Status = run.StatusSucceeded
	r.EndedAt = &ended
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	// The dispatcher commits the outcome from the run it holds in memory.
	if err := outcome.Commit(ctx, store.Audits(), store.Runs(), r, "system:dispatcher"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	entries, err := store.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var committed *audit.Entry
	for _, e := range entries {
		if e.Method == audit.MethodRun {
			committed = e
		}
	}
	if committed == nil {
		t.Fatal("no outcome entry was committed")
	}

	// A receipt rebuilds the record from the stored run, which is the only copy it has.
	stored, err := store.Runs().Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	rebuilt, err := outcome.Body(ctx, store.Runs(), stored)
	if err != nil {
		t.Fatalf("Body(stored) error = %v", err)
	}
	if !audit.VerifyContentDigest(committed.ContentDigest, committed.Nonce, rebuilt) {
		t.Errorf("the rebuilt outcome does not match the digest the chain committed, so every "+
			"receipt from this install reports a failed outcome:\n%s", rebuilt)
	}
}
