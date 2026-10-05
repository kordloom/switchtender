package dispatch

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// inventoryCapturingRunner records the content of the inventory file each run is handed, so a test
// can see which hosts a run actually executed against.
type inventoryCapturingRunner struct {
	// mu guards content.
	mu sync.Mutex
	// content is the inventory file content the most recent run was given.
	content string
}

// Run reads the materialized inventory file and records its content, then succeeds.
func (c *inventoryCapturingRunner) Run(_ context.Context, spec roundhouse.Spec,
	_ io.Writer) (roundhouse.Result, error) {
	if spec.Inventory != "" {
		if b, err := os.ReadFile(spec.Inventory); err == nil {
			c.mu.Lock()
			c.content = string(b)
			c.mu.Unlock()
		}
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// seen returns the inventory content the last run executed against.
func (c *inventoryCapturingRunner) seen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content
}

// TestApprovedRunIsPinnedToTheInventoryItWasApprovedWith covers an "approval releases exactly what
// was approved" gap on the single field that decides a change's blast radius: which hosts it runs
// against. A held run that targets a plain stored inventory is bound, at approval, to the
// inventory's id and nothing about its content. A composed inventory records the hosts it resolved
// to, and a git project pins its commit, but a plain stored inventory is materialized fresh from the
// store when the run executes. So after an approver releases a run scoped to one canary host,
// editing that inventory to list the whole fleet makes the approved run execute against every host
// in it, and the executor's approved-spec check cannot tell: the id did not move and no resolution
// was ever recorded.
//
// The safe outcomes are two: the run refuses because its inventory changed after approval, or it
// executes against exactly the hosts the approver was shown. This test accepts either and fails on
// the third, which is what happens today: the approved run silently runs against hosts nobody
// approved.
func TestApprovedRunIsPinnedToTheInventoryItWasApprovedWith(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const approvedHosts = "[web]\ncanary\n"
	const widenedHosts = "[web]\nprod1\nprod2\nprod3\n"

	invs := inventory.NewMemStore()
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_blast", Name: "web", Content: approvedHosts,
	}); err != nil {
		t.Fatalf("Save(inventory) error = %v", err)
	}

	store := run.NewMemStore()
	audits := audit.NewMemStore()
	runner := &inventoryCapturingRunner{}
	d := New(store, runner, nil,
		WithInventories(invs), WithAudits(audits), WithRunFilesRoot(t.TempDir()), WithNoJanitor())
	t.Cleanup(d.Close)

	held, err := d.Submit(ctx, "site.yml", "", run.WithTool(run.ToolAnsible),
		run.WithInventory("inv_blast"), run.WithRequireApproval(true),
		run.WithActor("operator-1"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("submitted status = %q, want pending_approval", held.Status)
	}

	// A person reviews the held run, scoped to the one canary host, and approves it. The approval
	// binds the spec.
	if _, err := d.Approve(ctx, held.ID, decider("approver-1", "session")); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	// After the approval, the inventory is edited to list the whole fleet. Editing an inventory is an
	// ordinary object mutation, not a change that goes back through the approval gate.
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_blast", Name: "web", Content: widenedHosts,
	}); err != nil {
		t.Fatalf("Save(widened inventory) error = %v", err)
	}

	// The claim loop takes the approved run and executes it.
	final := waitTerminal(t, store, held.ID)

	switch final.Status {
	case run.StatusFailed:
		if !strings.Contains(final.Error, "approved") {
			t.Fatalf("run failed for an unrelated reason: %q", final.Error)
		}
		// Refusing because the inventory changed after approval is a safe outcome.
		return
	case run.StatusSucceeded:
		// It ran. It must have run against the hosts the approver was shown, not the widened set.
	default:
		t.Fatalf("run ended %q (%s)", final.Status, final.Error)
	}

	got := runner.seen()
	if strings.Contains(got, "prod1") {
		t.Errorf("the approved run executed against the widened inventory %q, which the approver "+
			"never saw: an approval released a run scoped to canary and it ran against the fleet",
			strings.TrimSpace(got))
	}
}
