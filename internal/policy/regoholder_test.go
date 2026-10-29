package policy_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/policytest"
	"github.com/kordloom/switchtender/internal/run"
)

// holdModule is valid Rego that holds production commands, so the paid build compiles the same
// bundle these tests hold uncompiled.
var holdModule = policy.RegoModule{
	File:   "gate.rego",
	Source: "package switchtender\n\nhold contains \"production\" if contains(input.run.command, \"prod\")\n",
}

// storedWire returns the wire form of a stored program over holdModule with the given fields
// replaced, so a test can tamper with one field at a time.
func storedWire(t *testing.T, replace map[string]any) []byte {
	t.Helper()
	const pkg = "data.switchtender"
	doc := map[string]any{
		"package": pkg, "syntax": policy.RegoSyntaxV1, "modules": []policy.RegoModule{holdModule},
		"sha256": policy.RegoDigest(pkg, policy.RegoSyntaxV1, []policy.RegoModule{holdModule}),
	}
	for k, v := range replace {
		doc[k] = v
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// TestCommunityRefusesToCompileRegoNamingTeam pins the authoring-time refusal: a build without the
// paid module has no Rego compiler, so compiling a policy is refused with the tier's own line, inside
// ErrRego so the policy file reports it the way it reports any bad Rego entry.
func TestCommunityRefusesToCompileRegoNamingTeam(t *testing.T) {
	t.Parallel()
	_, err := policy.CompileRego("", "", []policy.RegoModule{holdModule})
	if !errors.Is(err, license.ErrRequiresTier) || !errors.Is(err, policy.ErrRego) {
		t.Fatalf("CompileRego() error = %v, want ErrRequiresTier inside ErrRego", err)
	}
	if !strings.Contains(err.Error(), "Team") {
		t.Errorf("CompileRego() error = %q, want it to name Team", err)
	}
}

// TestCommunityRefusesAPolicyFileWithRegoNamingTeam pins the same refusal where an operator meets
// it: a policy file declaring a Rego policy does not load, and the reason names the tier.
func TestCommunityRefusesAPolicyFileWithRegoNamingTeam(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gate.rego"), []byte(holdModule.Source), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	file := filepath.Join(dir, "policies.yml")
	doc := "rego:\n  - name: gate\n    files: [\"gate.rego\"]\n"
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	_, err := policy.NewFileStore(file)
	if !errors.Is(err, license.ErrRequiresTier) || !strings.Contains(err.Error(), "Team") {
		t.Fatalf("NewFileStore() error = %v, want the Team refusal", err)
	}
}

// TestAStoredRegoPolicyHoldsEveryRunOnCommunity pins the fail-closed reading. A Rego policy that
// arrives already written, from a backup or a control node, is kept with its bundle intact, and in a
// build that cannot evaluate it every run it might have held is held, with a reason naming the tier.
// It never refuses, never passes, and never notes, because any of those would claim to know what the
// bundle decided.
func TestAStoredRegoPolicyHoldsEveryRunOnCommunity(t *testing.T) {
	t.Parallel()
	prog := policytest.StoredRego(t, holdModule)
	if prog.Evaluable() {
		t.Fatal("Evaluable() = true in a build with no Rego compiler")
	}
	p := &policy.Policy{ID: "pol_gate", Name: "gate", MaxDestroy: policy.DisabledMaxDestroy,
		Rego: prog, RequireReason: "always"}
	set := []*policy.Policy{p}
	for _, r := range []*run.Run{
		{ID: "r0", Tool: run.ToolBash, Command: "deploy prod"},
		{ID: "r1", Tool: run.ToolBash, Command: "uptime"},
		{ID: "r2", Tool: run.ToolTerraform, Command: "infra", DryRun: true},
	} {
		d := prog.Decide(r)
		if d.Err != nil || len(d.Deny) > 0 || len(d.Note) > 0 || d.RequireDistinctApprover {
			t.Errorf("%s: Decide() = %+v, want a hold and nothing else", r.ID, d)
		}
		if diff := cmp.Diff([]string{policy.RegoUnavailableReason}, d.Hold); diff != "" {
			t.Errorf("%s: Decide().Hold mismatch (-want +got):\n%s", r.ID, diff)
		}
		if got := policy.Denying(set, r); got != nil {
			t.Errorf("%s: Denying() = %q, want nothing refused", r.ID, got.Name)
		}
		held := policy.Requiring(set, r)
		if held == nil {
			t.Fatalf("%s: Requiring() = nil, want the run held", r.ID)
		}
		if !strings.Contains(held.Name, policy.RegoUnavailableReason) ||
			!strings.Contains(held.Name, "rego sha256:"+prog.Digest()[:12]) {
			t.Errorf("%s: hold named %q, want the tier reason and the bundle digest", r.ID, held.Name)
		}
		if policy.RequireDistinct(set, r) {
			t.Errorf("%s: RequireDistinct() = true, want false", r.ID)
		}
		if got := policy.Noting(set, r); len(got) != 0 {
			t.Errorf("%s: Noting() = %v, want nothing noted", r.ID, got)
		}
		if got := policy.ReasonRequirement(set, r); got != "always" {
			t.Errorf("%s: ReasonRequirement() = %q, want the held run asked for a reason", r.ID, got)
		}
	}
	if !prog.Reads("plan") || !prog.Reads("anything") {
		t.Error("Reads() = false for a bundle this build cannot read, want true so callers prepare it")
	}
}

// TestAStoredRegoPolicyKeepsItsRecord pins that the in-force record of a stored program is what
// it was where the program came from: the digest, and any setting that differs from its default.
func TestAStoredRegoPolicyKeepsItsRecord(t *testing.T) {
	t.Parallel()
	var prog policy.RegoProgram
	if err := json.Unmarshal(storedWire(t, map[string]any{"timeout": "2s"}), &prog); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if prog.Timeout() != 2*time.Second || prog.Warn() != policy.RegoWarnHold {
		t.Errorf("settings = %s, %s, want 2s and hold", prog.Timeout(), prog.Warn())
	}
	set := policy.InForce([]*policy.Policy{{ID: "pol_gate", Name: "gate",
		MaxDestroy: policy.DisabledMaxDestroy, Rego: &prog}})
	if len(set.Rules) != 1 || !strings.Contains(set.Rules[0], "bundle sha256:"+prog.Digest()) ||
		!strings.Contains(set.Rules[0], "timeout 2s") {
		t.Errorf("InForce().Rules = %v, want the digest and the timeout named", set.Rules)
	}
	raw, err := json.Marshal(&prog)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var again policy.RegoProgram
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatalf("Unmarshal(round trip) error = %v", err)
	}
	if again.Digest() != prog.Digest() || again.Package() != prog.Package() ||
		again.Timeout() != prog.Timeout() || again.Warn() != prog.Warn() {
		t.Errorf("round trip changed the program: %s %s %s %s", again.Digest(), again.Package(),
			again.Timeout(), again.Warn())
	}
}

// TestAStoredRegoPolicyThatCannotBeTrustedIsRefused pins that holding a bundle uncompiled is not a
// way past its checks: a digest that does not match, a setting no build honors, and a program with
// no package are refused on every build.
func TestAStoredRegoPolicyThatCannotBeTrustedIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Replace are the wire fields changed.
		Replace map[string]any
		// WantText is part of the refusal.
		WantText string
	}{{ // Test 0: A tampered digest.
		Replace: map[string]any{"sha256": strings.Repeat("0", 64)}, WantText: "digest",
	}, { // Test 1: A warn mode no build knows.
		Replace: map[string]any{"warn": "shout"}, WantText: "warn must be",
	}, { // Test 2: A timeout past the cap.
		Replace: map[string]any{"timeout": "1h"}, WantText: "timeout must be",
	}, { // Test 3: A timeout that is not a duration.
		Replace: map[string]any{"timeout": "soon"}, WantText: "not a duration",
	}, { // Test 4: No package, so the digest cannot be checked.
		Replace: map[string]any{"package": ""}, WantText: "names no package",
	}, { // Test 5: A syntax no build knows.
		Replace: map[string]any{"syntax": "v9"}, WantText: "syntax must be",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var prog policy.RegoProgram
			err := json.Unmarshal(storedWire(t, test.Replace), &prog)
			if !errors.Is(err, policy.ErrRego) || !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("Unmarshal() error = %v, want ErrRego naming %q", err, test.WantText)
			}
		})
	}
}
