package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestPoCFilePolicyFailsOpen proves a policy file that becomes unreadable after startup silently
// disables every approval gate instead of refusing the run.
func TestPoCFilePolicyFailsOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policies.yaml")
	body := "policies:\n  - name: all-ansible\n    tool: ansible\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}

	runs := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01", "web02"}}
	d := New(runs, runner, nil, WithPolicies(store))
	defer d.Close()
	ctx := context.Background()

	held, err := d.Submit(ctx, "site.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("baseline status = %q, want pending_approval", held.Status)
	}

	// A truncated write, a botched deploy, or an rm. Nothing else about the install changed.
	if err := os.WriteFile(path, []byte("policies:\n  - name: all-ansible\n    tool: ans"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	after, err := d.Submit(ctx, "site.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if after.Status != run.StatusPendingApproval {
		t.Fatalf("FAIL-OPEN: status = %q after the policy file broke; the gate vanished", after.Status)
	}
}
