package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

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

// TestARefusedDecisionLeavesNoChainEntry proves a decision the dispatcher refuses is not recorded.
//
// The precondition used to be the compare-and-set at the end of Approve and Reject, which runs after
// the chain entry is appended. A decision arriving late was refused to the caller and written to the
// chain anyway, and the consequences were worse than a duplicate row:
//
//   - A second approve after the run finished put an approval entry AFTER the run's own outcome
//     entry. verifyTimeOrder reads an approval that postdates the execution it authorized as the
//     signature of a gate bypass, so an honest install's bundle reported NOT VERIFIED because an
//     approver double-clicked.
//   - A reject arriving after the run was approved and succeeded was refused with 409 and still
//     wrote DECISION /decision/rejected, so the permanent record said an approver rejected a run
//     that ran and succeeded.
func TestARefusedDecisionLeavesNoChainEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	decisionsFor := func(t *testing.T, audits audit.Store, id string) []string {
		t.Helper()
		entries, err := audits.List(ctx, 1000)
		if err != nil {
			t.Fatalf("audit List() error = %v", err)
		}
		var got []string
		for _, e := range entries {
			if e.Method == audit.MethodDecision && strings.Contains(e.Path, id) {
				got = append(got, e.Path)
			}
		}
		return got
	}

	tests := []struct {
		// Name says which late decision is being refused.
		Name string
		// Second is the call made after the run has already been approved and has finished.
		Second func(d *Dispatcher, id string) error
	}{{ // Test 0: A second approval, the double-click.
		Name: "a second approve",
		Second: func(d *Dispatcher, id string) error {
			_, err := d.Approve(ctx, id, "approver-pat", "session")
			return err
		},
	}, { // Test 1: A reject a moment too late, after the approval already released the run.
		Name: "a late reject",
		Second: func(d *Dispatcher, id string) error {
			_, err := d.Reject(ctx, id, "changed my mind", "approver-pat", "session")
			return err
		},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			d := New(store, okRunner(), nil, WithAudits(audits), WithNoJanitor())
			defer d.Close()

			held := &run.Run{
				ID: "run_late_" + fmt.Sprint(testNum), Playbook: "site.yml", Inventory: "prod",
				Status: run.StatusPendingApproval, CreatedAt: time.Now(), Actor: "operator",
			}
			if err := store.Save(ctx, held); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if _, err := d.Approve(ctx, held.ID, "approver-pat", "session"); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			waitTerminal(t, store, held.ID)

			before := decisionsFor(t, audits, held.ID)
			if len(before) != 1 {
				t.Fatalf("%s: the approval recorded %d decisions, want exactly 1: %v",
					test.Name, len(before), before)
			}

			if err := test.Second(d, held.ID); !errors.Is(err, ErrNotPendingApproval) {
				t.Errorf("%s: err = %v, want ErrNotPendingApproval", test.Name, err)
			}
			after := decisionsFor(t, audits, held.ID)
			if diff := cmp.Diff(before, after); diff != "" {
				t.Errorf("%s: a refused decision changed the chain (-before +after):\n%s",
					test.Name, diff)
			}
		})
	}
}
