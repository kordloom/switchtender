package outcome_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestTheSpecBindingSurvivesTheStoreRoundTrip covers the failure this binding could cause if it
// were wrong, which is worse than the tamper it prevents.
//
// The binding is stamped from the run an approver decided on and recomputed at execution from the
// row the executor loaded. If those two reduce to different bytes for a run nobody touched, every
// approved run on the install is refused as tampered and nothing governed can execute at all. That
// is a fleet-wide outage produced by a safety check, so it is pinned rather than assumed.
//
// The fields exercised are the ones a store round trip is most likely to move: a timestamp from a
// zone west of Greenwich, launch variables of several JSON types including a nested object and an
// array, tags and credentials as lists, and a shard count behind a pointer.
func TestTheSpecBindingSurvivesTheStoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	zone := time.FixedZone("CST", -6*60*60)
	created := time.Date(2026, 8, 17, 9, 30, 15, 123456789, zone)
	shards := 4
	r := &run.Run{
		ID: "run_bind", Playbook: "site.yml", Inventory: "prod", Tool: run.ToolAnsible,
		Status: run.StatusPendingApproval, CreatedAt: created,
		Limit: "web*", Tags: []string{"deploy", "migrate"}, SkipTags: []string{"slow"},
		CredentialIDs: []string{"cred_a", "cred_b"}, PullCredentialID: "cred_reg",
		ProjectID: "prj_1", InventoryID: "inv_1", ShardCount: &shards,
		Verbosity: 2, Forks: 10, DiffMode: true, Timeout: 1800,
		ExtraVars: map[string]any{
			"retries":  3,
			"ratio":    0.25,
			"enabled":  true,
			"name":     "nightly",
			"nothing":  nil,
			"list":     []any{"a", "b"},
			"nested":   map[string]any{"deep": "value", "n": 7},
			"password": "hunter2",
		},
		Steps: []run.PipelineStep{{Name: "one", Tool: run.ToolBash, Command: "echo hi"}},
	}

	// What the approver decided on, computed from the run held in memory.
	atApproval, err := outcome.SpecBinding(r)
	if err != nil {
		t.Fatalf("SpecBinding(in memory) error = %v", err)
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// What the executor recomputes, from the only copy it has.
	stored, err := store.Runs().Get(ctx, "run_bind")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	atExecution, err := outcome.SpecBinding(stored)
	if err != nil {
		t.Fatalf("SpecBinding(stored) error = %v", err)
	}

	if atApproval != atExecution {
		t.Errorf("the binding stamped at approval is %s and the executor recomputes %s for a run "+
			"nothing touched, so every approved run on this install is refused as tampered and "+
			"nothing governed can execute", atApproval, atExecution)
	}

	// And the disclosed digest keeps the same property, since the gate checks it too.
	da, err := outcome.SpecDigest(r)
	if err != nil {
		t.Fatalf("SpecDigest(in memory) error = %v", err)
	}
	db, err := outcome.SpecDigest(stored)
	if err != nil {
		t.Fatalf("SpecDigest(stored) error = %v", err)
	}
	if da != db {
		t.Errorf("the disclosed spec digest does not survive the store round trip: %s then %s",
			da, db)
	}
}
