package policy_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// writePolicyFile writes a policy file and returns its path.
func writePolicyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// TestFileStoreLoadsAndGates verifies a policy written in a file gates the runs it names.
func TestFileStoreLoadsAndGates(t *testing.T) {
	t.Parallel()
	path := writePolicyFile(t, `
policies:
  - name: prod-terraform-destroy
    tool: terraform
    command_contains: destroy
  - name: large-teardown
    tool: opentofu
    max_destroy: 5
`)
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	all, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("loaded %d policies, want 2", len(all))
	}
	// An omitted threshold must mean a blanket policy. Defaulting the other way turns a gate an
	// operator wrote as absolute into one that only fires on a large destroy.
	if all[0].MaxDestroy != policy.DisabledMaxDestroy {
		t.Errorf("policy with no max_destroy has MaxDestroy = %d, want it disabled so the policy is "+
			"a blanket gate", all[0].MaxDestroy)
	}
	if all[1].MaxDestroy != 5 {
		t.Errorf("max_destroy = %d, want 5", all[1].MaxDestroy)
	}
	gated := &run.Run{Tool: "terraform", Command: "terraform destroy prod"}
	if !policy.Requires(all, gated) {
		t.Error("a terraform destroy is not held by a policy file that names it")
	}
	if policy.Requires(all, &run.Run{Tool: "ansible", Playbook: "site.yml"}) {
		t.Error("an unrelated run is held by a policy that should not match it")
	}
}

// TestFileStoreRefusesWrites verifies the file is the source of truth, so a policy change made
// through the API is rejected rather than written somewhere nobody is reading.
//
// A policy change that appears to succeed and has no effect is worse than one that fails: the
// operator believes a gate exists and it does not.
func TestFileStoreRefusesWrites(t *testing.T) {
	t.Parallel()
	store, err := policy.NewFileStore(writePolicyFile(t, "policies: []\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, policy.NewPolicy("sneaky")); !errors.Is(err, policy.ErrReadOnly) {
		t.Errorf("Save() error = %v, want ErrReadOnly", err)
	}
	if err := store.Delete(ctx, "pol_anything"); !errors.Is(err, policy.ErrReadOnly) {
		t.Errorf("Delete() error = %v, want ErrReadOnly", err)
	}
}

// TestFileStoreRefusesAMalformedFile verifies a bad file fails loudly instead of loading zero
// policies, which would leave an install where nothing is gated and nothing says so.
func TestFileStoreRefusesAMalformedFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body string
	}{
		{Name: "not yaml", Body: "policies: [ this is not: valid: yaml\n"},
		{Name: "policy with no name", Body: "policies:\n  - tool: terraform\n"},
		{Name: "unknown tool", Body: "policies:\n  - name: x\n    tool: kubernetes\n"},
		{Name: "negative threshold", Body: "policies:\n  - name: x\n    max_destroy: -1\n"},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if _, err := policy.NewFileStore(writePolicyFile(t, test.Body)); err == nil {
				t.Error("a malformed policy file loaded, so the server would start with no gates " +
					"and no warning")
			}
		})
	}
	if _, err := policy.NewFileStore(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing policy file loaded, so a typo in the path silently disables every gate")
	}
}

// TestFileStoreRereadsAfterAChange verifies a merged change takes effect without a restart, which
// is the point of keeping policies in a repository.
func TestFileStoreRereadsAfterAChange(t *testing.T) {
	t.Parallel()
	path := writePolicyFile(t, "policies:\n  - name: first\n    tool: terraform\n")
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	ctx := context.Background()
	if all, _ := store.List(ctx); len(all) != 1 {
		t.Fatalf("loaded %d policies, want 1", len(all))
	}
	if err := os.WriteFile(path,
		[]byte("policies:\n  - name: first\n    tool: terraform\n  - name: second\n    tool: bash\n"),
		0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() after edit error = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("after editing the file the store serves %d policies, want 2: a merged policy "+
			"change has to take effect without a restart", len(all))
	}
}

// TestFilePolicyIDIsStable verifies an id survives reordering and reloading, so an approval recorded
// against a policy still resolves after the file is edited.
func TestFilePolicyIDIsStable(t *testing.T) {
	t.Parallel()
	first, err := policy.NewFileStore(writePolicyFile(t,
		"policies:\n  - name: alpha\n    tool: bash\n  - name: beta\n    tool: go\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	swapped, err := policy.NewFileStore(writePolicyFile(t,
		"policies:\n  - name: beta\n    tool: go\n  - name: alpha\n    tool: bash\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	ctx := context.Background()
	a, _ := first.List(ctx)
	b, _ := swapped.List(ctx)
	if a[0].ID != b[1].ID || a[1].ID != b[0].ID {
		t.Error("reordering the file changed policy ids, so an approval recorded against one no " +
			"longer resolves")
	}
	if got, err := first.Get(ctx, a[0].ID); err != nil || got.Name != "alpha" {
		t.Errorf("Get(%s) = (%v, %v), want the alpha policy", a[0].ID, got, err)
	}
}

// TestFileStoreLoadsActorRules confirms the file schema carries the actor, risk, and effect
// vocabulary, and that a rule with an unknown word in it refuses to load rather than silently
// matching nothing.
func TestFileStoreLoadsActorRules(t *testing.T) {
	t.Parallel()
	path := writePolicyFile(t, `
policies:
  - name: agent-destructive-deny
    actor_kind: agent
    command_contains: "drop database"
    effect: deny
  - name: agent-high-risk-approval
    actor_kind: agent
    min_risk: high
`)
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	all, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("loaded %d policies, want 2", len(all))
	}
	agentDrop := &run.Run{Tool: "bash", Command: "psql -c 'drop database prod'", ActorType: "agent"}
	if got := policy.Denying(all, agentDrop); got == nil || got.Name != "agent-destructive-deny" {
		t.Errorf("Denying() = %v, want the deny rule from the file", got)
	}
	risky := &run.Run{Tool: "bash", Command: "rm -rf /srv/data", ActorType: "agent"}
	if got := policy.Requiring(all, risky); got == nil || got.Name != "agent-high-risk-approval" {
		t.Errorf("Requiring() = %v, want the high-risk rule from the file", got)
	}

	bad := writePolicyFile(t, `
policies:
  - name: typo
    effect: denny
`)
	if _, err := policy.NewFileStore(bad); err == nil {
		t.Error("NewFileStore() accepted an unknown effect")
	}
}
