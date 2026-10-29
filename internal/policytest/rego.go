package policytest

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
)

// StoredRego returns a Rego program the way a control node or a backup hands one over: decoded from
// its wire form, with the digest computed from the modules. Every build can build one, so a test
// that needs a Rego policy in a set, in a store, or in a backup does not need the paid compiler. The
// modules must be valid Rego in package switchtender, so the paid build compiles the same program.
// With no module given it uses one that holds every run.
func StoredRego(t *testing.T, modules ...policy.RegoModule) *policy.RegoProgram {
	t.Helper()
	if len(modules) == 0 {
		modules = []policy.RegoModule{{
			File: "gate.rego", Source: "package switchtender\n\nhold contains \"gated\" if true\n",
		}}
	}
	const pkg = "data.switchtender"
	raw, err := json.Marshal(map[string]any{
		"package": pkg, "syntax": policy.RegoSyntaxV1, "modules": modules,
		"sha256": policy.RegoDigest(pkg, policy.RegoSyntaxV1, modules),
	})
	if err != nil {
		t.Fatalf("encode the stored program: %v", err)
	}
	var prog policy.RegoProgram
	if err := json.Unmarshal(raw, &prog); err != nil {
		t.Fatalf("decode the stored program: %v", err)
	}
	return &prog
}

// DocBlocks returns the indented code blocks of the Markdown page at path, each with its
// indentation removed.
func DocBlocks(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.TrimSpace(strings.Join(cur, "\n"))+"\n")
			cur = nil
		}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "    "):
			cur = append(cur, strings.TrimPrefix(line, "    "))
		case strings.TrimSpace(line) == "" && len(cur) > 0:
			cur = append(cur, "")
		default:
			flush()
		}
	}
	flush()
	return blocks
}
