package relay_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRelayWorkerReceivesTheRegoBundle checks that a Rego policy crosses the relay compiled and
// deciding as it does on the control node. The plan gate runs where the run executes, so a worker
// that received the policy without its bundle would either plan nothing or, reading an empty rule,
// hold everything; neither is the policy in force.
//
// It does not run in parallel: the control node's file store checks the process license, which
// this test sets to Team and restores.
func TestRelayWorkerReceivesTheRegoBundle(t *testing.T) {
	prev := license.Current()
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_relay", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	t.Cleanup(func() { license.Set(prev) })

	dir := t.TempDir()
	module := "package switchtender\n\nplan_gate if input.run.tool == \"terraform\"\n\n" +
		"hold contains \"big teardown\" if {\n\tinput.plan.planned\n\tinput.plan.destroys > 5\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "gate.rego"), []byte(module), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	path := filepath.Join(dir, "policies.yml")
	if err := os.WriteFile(path, []byte("rego:\n  - name: teardown\n    files: [gate.rego]\n"),
		0o600); err != nil {
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
	if len(remote) != 1 || remote[0].Rego == nil ||
		remote[0].Rego.Digest() != local[0].Rego.Digest() {
		t.Fatalf("policies across the relay = %+v, want the Rego bundle intact", remote)
	}
	apply := &run.Run{ID: "r", Tool: run.ToolTerraform, Command: "infra/prod"}
	if !policy.PlanGated(remote, apply) {
		t.Error("the worker's copy does not plan the apply the control node's copy plans")
	}
	seven := 7
	proposal := &run.Run{ID: "p", Tool: run.ToolTerraform, Command: "infra/prod",
		ProposedFrom: "r", PlanDestroys: &seven}
	if policy.Requiring(remote, proposal) == nil {
		t.Error("the worker's copy does not hold the apply the control node's copy holds")
	}
}
