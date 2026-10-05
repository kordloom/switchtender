package relay_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRelayWorkerReceivesTheRegoSettings checks that a Rego policy's warn and timeout settings
// cross the relay with its bundle. A worker that received the bundle without them would hold a run
// the control node notes, and evaluate under a limit nobody set, so the same run would be judged two
// ways depending on which process claimed it.
//
// It does not run in parallel: the control node's file store checks the process license, which
// this test sets to Team and restores.
func TestRelayWorkerReceivesTheRegoSettings(t *testing.T) {
	prev := license.Current()
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_relay", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	t.Cleanup(func() { license.Set(prev) })

	dir := t.TempDir()
	module := "package switchtender\n\n" +
		"warn contains \"no change ticket\" if not input.run.labels.ticket\n"
	if err := os.WriteFile(filepath.Join(dir, "advice.rego"), []byte(module), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	path := filepath.Join(dir, "policies.yml")
	if err := os.WriteFile(path, []byte("rego:\n  - name: advice\n    files: [advice.rego]\n"+
		"    warn: note\n    timeout: 2s\n"), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	backing, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	local, err := backing.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	remote, err := newPolicyRelay(t, backing).List(context.Background())
	if err != nil {
		t.Fatalf("List() across the relay error = %v", err)
	}
	if len(remote) != 1 || remote[0].Rego == nil {
		t.Fatalf("policies across the relay = %+v, want the Rego policy", remote)
	}
	if got := remote[0].Rego; got.Warn() != policy.RegoWarnNote || got.Timeout() != 2*time.Second {
		t.Errorf("the worker's copy has warn %s and timeout %s, want note and 2s", got.Warn(),
			got.Timeout())
	}
	r := &run.Run{ID: "r", Tool: run.ToolBash, Command: "uptime"}
	if policy.Requiring(remote, r) != nil {
		t.Error("the worker's copy holds a run the control node's copy only notes")
	}
	if diff := cmp.Diff(policy.Noting(local, r), policy.Noting(remote, r)); diff != "" {
		t.Errorf("the worker's copy notes differently (-control +worker):\n%s", diff)
	}
}
