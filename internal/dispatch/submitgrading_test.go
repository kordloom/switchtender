package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestSubmitHoldsADestructivePlaybook drives the gate through the path every submission takes.
//
// Every way a run is born reaches the executor through Submit: the API, a template launch, a
// schedule firing, and the drift reconcile that proposes a fix. Each was checked by reading the
// code and seeing it call the graded helper, which is how the four paths that were not graded got
// missed in the first place.
//
// This proves it through the dispatcher instead, on a playbook whose destruction is inside a file
// the run only names.
func TestSubmitHoldsADestructivePlaybook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	wipe := filepath.Join(dir, "wipe.yml")
	if err := os.WriteFile(wipe, []byte(`
- hosts: all
  tasks:
    - ansible.builtin.file:
        path: /srv/archive
        state: absent
`), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	restart := filepath.Join(dir, "restart.yml")
	if err := os.WriteFile(restart, []byte(`
- hosts: all
  tasks:
    - ansible.builtin.service:
        name: nginx
        state: restarted
`), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{
		ID: "pol_undo", Name: "hold the permanent", MaxDestroy: -1,
		Effect: policy.EffectRequireApproval, Reversibility: run.Irreversible,
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithPolicies(policies))
	t.Cleanup(d.Close)

	held, err := d.Submit(ctx, wipe, "hosts.ini", run.WithTool(run.ToolAnsible))
	if err != nil {
		t.Fatalf("Submit(destructive): %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Errorf("a playbook that deletes an archive submitted as %s, want pending_approval. The "+
			"gate is reading the run rather than what the run will do, so the rule never fires on "+
			"the tool this product exists to run", held.Status)
	}
	if held.HeldByPolicy == "" {
		t.Error("the held run does not name the rule that held it")
	}

	// And a recoverable playbook is left alone, or the rule fires on everything and gets removed.
	free, err := d.Submit(ctx, restart, "hosts.ini", run.WithTool(run.ToolAnsible))
	if err != nil {
		t.Fatalf("Submit(recoverable): %v", err)
	}
	if free.Status == run.StatusPendingApproval {
		t.Error("a service restart was held by an irreversible rule. A gate that fires on " +
			"recoverable work is one an operator switches off")
	}
}
