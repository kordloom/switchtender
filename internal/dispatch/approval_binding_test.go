package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestApproveCommitsTheDecision proves an approval is a chain event bound to content: releasing a
// held run appends a DECISION entry naming the approver, committing a digest of the exact spec
// released, and stamps that digest on the run for the executor to enforce.
func TestApproveCommitsTheDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	d := New(store, okRunner(), nil, WithAudits(audits), WithNoJanitor())
	defer d.Close()

	held := &run.Run{
		ID: "run_bound", Playbook: "site.yml", Inventory: "prod",
		Status: run.StatusPendingApproval, CreatedAt: time.Now(),
		Actor: "prod-remediator", ActorType: "agent",
		ExtraVars: map[string]any{"service": "api"},
	}
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	approved, err := d.Approve(ctx, held.ID, "approver-pat", "session")
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	wantDigest, err := outcome.SpecDigest(held)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	if approved.ApprovedSpecDigest != wantDigest {
		t.Errorf("ApprovedSpecDigest = %q, want %q", approved.ApprovedSpecDigest, wantDigest)
	}
	stored, err := store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.ApprovedSpecDigest != wantDigest {
		t.Errorf("stored ApprovedSpecDigest = %q, want %q", stored.ApprovedSpecDigest, wantDigest)
	}

	entries, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var decision *audit.Entry
	for _, e := range entries {
		if e.Method == audit.MethodDecision && strings.HasPrefix(e.Path, "/runs/run_bound/decision/") {
			decision = e
		}
	}
	if decision == nil {
		t.Fatal("approval left no DECISION entry on the chain")
	}
	if decision.Actor != "approver-pat" || decision.ActorType != "session" {
		t.Errorf("decision actor = %s (%s), want approver-pat (session)", decision.Actor, decision.ActorType)
	}
	body, _, err := outcome.DecisionBody(held, "approved")
	if err != nil {
		t.Fatalf("DecisionBody() error = %v", err)
	}
	if !audit.VerifyContentDigest(decision.ContentDigest, decision.Nonce, body) {
		t.Error("the decision entry's digest does not commit the rebuilt decision body")
	}
}

// TestExecutorRefusesASpecChangedAfterApproval proves the tripwire: a run whose spec no longer
// reduces to the digest the approver bound to fails with that stated, rather than executing
// something nobody approved.
func TestExecutorRefusesASpecChangedAfterApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor())
	defer d.Close()

	r := &run.Run{
		ID: "run_drifted", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: time.Now(), ApprovedSpecDigest: "sha256:not-what-this-spec-reduces-to",
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got := d.execute(ctx, r); got != run.StatusFailed {
		t.Fatalf("execute() = %q, want failed", got)
	}
	stored, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.Status != run.StatusFailed || !strings.Contains(stored.Error, "changed after it was approved") {
		t.Errorf("run = %q error %q, want a failed run stating the spec changed", stored.Status, stored.Error)
	}
}
