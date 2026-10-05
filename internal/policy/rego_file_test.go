package policy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
)

// regoPolicyFile is a policy file holding one YAML rule and one Rego policy built from two modules,
// the decision module and a helper library beside it.
const regoPolicyFile = `policies:
  - name: hold bash
    tool: bash
rego:
  - name: guardrails
    files: [rego/guardrails.rego, rego/lib.rego]
`

// guardrailsModule is the decision module regoPolicyFile loads.
const guardrailsModule = `package switchtender

import data.lib

deny contains "agents may not touch production" if {
	input.actor.kind == "agent"
	lib.prod
}
`

// libModule is the helper regoPolicyFile loads beside the decision module.
const libModule = `package lib

prod if input.run.labels.env == "prod"
`

// writeRegoFiles lays out a policy file and its modules in dir and returns the policy file's path.
func writeRegoFiles(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "policies.yml")
}

// TestAPolicyFileLoadsRegoBesideItsYAML pins the load path: the Rego policy is read from paths
// relative to the policy file, listed after the YAML rules, and decides with its helper module.
func TestAPolicyFileLoadsRegoBesideItsYAML(t *testing.T) {
	t.Parallel()
	path := writeRegoFiles(t, t.TempDir(), map[string]string{
		"policies.yml": regoPolicyFile, "rego/guardrails.rego": guardrailsModule,
		"rego/lib.rego": libModule,
	})
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	all, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 2 || all[0].Rego != nil || all[1].Rego == nil {
		t.Fatalf("List() = %+v, want the YAML rule then the Rego policy", all)
	}
	if all[1].ID != filePolicyID("guardrails") || all[1].MaxDestroy != DisabledMaxDestroy {
		t.Errorf("Rego policy = %+v, want a stable id and no destroy threshold", all[1])
	}
	agent := &run.Run{ID: "r", Tool: "python", Command: "x", ActorType: "agent",
		Labels: map[string]string{"env": "prod"}}
	if Denying(all, agent) == nil {
		t.Error("the Rego policy did not refuse an agent's production run")
	}
	agent.Labels["env"] = "dev"
	if Denying(all, agent) != nil {
		t.Error("the Rego policy refused an agent's development run")
	}
	got, err := store.Get(context.Background(), filePolicyID("guardrails"))
	if err != nil || got.Rego == nil {
		t.Errorf("Get(rego) = %+v, %v, want the Rego policy", got, err)
	}
}

// TestEditingARegoModuleReloadsTheSet pins that a module edit takes effect without a restart, as an
// edit to the YAML does, and that an edit which breaks the module fails closed: the store stops
// answering rather than serving the last good copy or no policy at all.
func TestEditingARegoModuleReloadsTheSet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeRegoFiles(t, dir, map[string]string{
		"policies.yml": regoPolicyFile, "rego/guardrails.rego": guardrailsModule,
		"rego/lib.rego": libModule,
	})
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	before, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	lib := filepath.Join(dir, "rego", "lib.rego")
	// The edit changes the size, so the reload does not depend on timestamp granularity.
	if err := os.WriteFile(lib, []byte("package lib\n\nprod if input.run.queue == \"production\"\n"),
		0o600); err != nil {
		t.Fatalf("edit module: %v", err)
	}
	after, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() after edit error = %v", err)
	}
	if after[1].Rego.Digest() == before[1].Rego.Digest() {
		t.Fatal("editing a helper module did not reload the Rego policy")
	}
	if InForce(after).Digest == InForce(before).Digest {
		t.Error("editing a helper module left the in-force digest unchanged")
	}

	if err := os.WriteFile(lib, []byte("package lib\n\nprod if {"), 0o600); err != nil {
		t.Fatalf("break module: %v", err)
	}
	if _, err := store.List(context.Background()); !errors.Is(err, ErrRego) {
		t.Errorf("List() after a broken edit error = %v, want ErrRego", err)
	}
	if err := os.Remove(lib); err != nil {
		t.Fatalf("remove module: %v", err)
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Error("List() served policies after a module it loads was deleted")
	}
}

// TestAPolicyFileWithABadRegoEntryDoesNotLoad pins the refusals the file adds around a Rego
// policy, each of which would otherwise leave a policy that decides nothing.
func TestAPolicyFileWithABadRegoEntryDoesNotLoad(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// File is the policy file.
		File string
		// WantText is a fragment the refusal must carry.
		WantText string
	}{{ // Test 0: No name.
		File:     "rego:\n  - files: [g.rego]\n",
		WantText: "has no name",
	}, { // Test 1: No files.
		File:     "rego:\n  - name: empty\n",
		WantText: "names no files",
	}, { // Test 2: A file that is not there.
		File:     "rego:\n  - name: missing\n    files: [nope.rego]\n",
		WantText: "nope.rego",
	}, { // Test 3: A name another policy already has.
		File:     "policies:\n  - name: same\nrego:\n  - name: same\n    files: [g.rego]\n",
		WantText: "shares its name",
	}, { // Test 4: A module that does not compile.
		File:     "rego:\n  - name: broken\n    files: [bad.rego]\n",
		WantText: "parse",
	}, { // Test 5: A syntax this build does not know.
		File:     "rego:\n  - name: odd\n    files: [g.rego]\n    syntax: v9\n",
		WantText: "syntax",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := writeRegoFiles(t, t.TempDir(), map[string]string{
				"policies.yml": test.File,
				"g.rego":       "package switchtender\n\nhold contains \"x\" if true\n",
				"bad.rego":     "package switchtender\n\nhold contains if {\n",
			})
			_, err := NewFileStore(path)
			if err == nil {
				t.Fatal("NewFileStore() loaded a file it should refuse")
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("NewFileStore() error = %v, want it to mention %q", err, test.WantText)
			}
		})
	}
}

// TestRegoIsGatedAsTheFullPolicyEngine pins the license line. A Rego policy is the full engine, so
// Community and Pro refuse a file carrying one, naming the Rego policy, exactly as they refuse a
// deny rule, while what each tier could load before is unchanged. Team serves it, and a lapsed Team
// keeps serving it, since a lapse takes nothing.
//
// It does not run in parallel: it swaps the process license, which every other test here reads.
func TestRegoIsGatedAsTheFullPolicyEngine(t *testing.T) {
	dir := t.TempDir()
	withRego := writeRegoFiles(t, dir, map[string]string{
		"policies.yml": "rego:\n  - name: guardrails\n    files: [g.rego]\n",
		"g.rego":       "package switchtender\n\nhold contains \"x\" if true\n",
	})
	plain := writeRegoFiles(t, t.TempDir(), map[string]string{
		"policies.yml": "policies:\n  - name: hold bash\n    tool: bash\n",
	})
	five := writeRegoFiles(t, t.TempDir(), map[string]string{
		"policies.yml": "policies:\n" +
			"  - name: a\n  - name: b\n  - name: c\n  - name: d\n  - name: e\n",
	})
	at := func(tier, expires string) *license.License {
		return &license.License{Claims: license.Claims{
			V: 1, ID: "lic_rego", Org: "test", Tier: tier,
			Issued: "2026-01-01T00:00:00Z", Expires: expires,
		}}
	}
	live := time.Now().AddDate(1, 0, 0).UTC().Format(time.RFC3339)
	lapsed := time.Now().AddDate(0, 0, -1).UTC().Format(time.RFC3339)
	team := license.Current()
	t.Cleanup(func() { license.Set(team) })

	tests := []struct {
		// License is the license in force, nil for Community.
		License *license.License
		// Path is the policy file read.
		Path string
		// WantRefused is whether the read is refused.
		WantRefused bool
	}{{ // Test 0: Community refuses Rego.
		License: nil, Path: withRego, WantRefused: true,
	}, { // Test 1: Pro refuses Rego.
		License: at(license.TierPro, live), Path: withRego, WantRefused: true,
	}, { // Test 2: Team serves Rego.
		License: at(license.TierTeam, live), Path: withRego, WantRefused: false,
	}, { // Test 3: Enterprise serves Rego.
		License: at(license.TierEnterprise, live), Path: withRego, WantRefused: false,
	}, { // Test 4: A lapsed Team keeps serving what it served.
		License: at(license.TierTeam, lapsed), Path: withRego, WantRefused: false,
	}, { // Test 5: Community still serves its one plain rule.
		License: nil, Path: plain, WantRefused: false,
	}, { // Test 6: Pro still serves five plain rules.
		License: at(license.TierPro, live), Path: five, WantRefused: false,
	}}
	for testNum, test := range tests {
		license.Set(test.License)
		store, err := NewFileStore(test.Path)
		if err != nil {
			t.Fatalf("test %d: NewFileStore() error = %v", testNum, err)
		}
		_, err = store.List(context.Background())
		if refused := err != nil; refused != test.WantRefused {
			t.Errorf("test %d: refused = %v (%v), want %v", testNum, refused, err, test.WantRefused)
			continue
		}
		if test.WantRefused && (!strings.Contains(err.Error(), `Rego policy "guardrails"`) ||
			!strings.Contains(err.Error(), "switchtender.com/pricing")) {
			t.Errorf("test %d: refusal %q does not name the Rego policy and where to go",
				testNum, err)
		}
	}
}
