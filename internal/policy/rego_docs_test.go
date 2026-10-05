package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// docBlocks returns the indented code blocks of a Markdown page, each with its indentation removed.
func docBlocks(t *testing.T, page string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", page))
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
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

// TestThePolicyPageExamplesAreTrue compiles every Rego module the policy page shows and decodes
// its sample input, so the page cannot show a module this build refuses or an input that is not
// the document a policy is evaluated against.
func TestThePolicyPageExamplesAreTrue(t *testing.T) {
	t.Parallel()
	var modules, inputs int
	for _, block := range docBlocks(t, "policy.md") {
		switch {
		case strings.HasPrefix(block, "package switchtender"):
			modules++
			files := []RegoModule{{File: "doc.rego", Source: block}}
			if strings.Contains(block, "import data.lib") {
				files = append(files, RegoModule{File: "lib.rego",
					Source: "package lib\n\nprod if input.run.labels.env == \"prod\"\n"})
			}
			if _, err := CompileRego("", "", files); err != nil {
				t.Errorf("the page shows a module this build refuses: %v\n%s", err, block)
			}
		case strings.HasPrefix(block, "{"):
			inputs++
			var doc map[string]any
			if err := json.Unmarshal([]byte(block), &doc); err != nil {
				t.Fatalf("the sample input is not JSON: %v", err)
			}
			got := schemaPaths("input", anyShape(doc))
			want := schemaPaths("input", anyShape(RegoInput(&run.Run{})))
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the sample input is not the input document (-document +sample):\n%s", diff)
			}
		}
	}
	if modules < 2 || inputs != 1 {
		t.Errorf("found %d modules and %d inputs on the page, want at least 2 and exactly 1",
			modules, inputs)
	}
}

// TestTheSentinelPortDecidesAsDocumented runs the Rego the migration note shows against the runs it
// describes: a large destroy on a weekday is refused, and the same destroy on a weekend is not.
func TestTheSentinelPortDecidesAsDocumented(t *testing.T) {
	t.Parallel()
	var src string
	for _, block := range docBlocks(t, "policy.md") {
		if strings.Contains(block, "weekend") && strings.HasPrefix(block, "package switchtender") {
			src = block
		}
	}
	if src == "" {
		t.Fatal("the page has no Sentinel port to check")
	}
	prog, err := CompileRego("", "", []RegoModule{{File: "sentinel.rego", Source: src}})
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	set := []*Policy{{ID: "pol_s", Name: "window", MaxDestroy: DisabledMaxDestroy, Rego: prog}}
	seven := 7
	apply := func(at string) *run.Run {
		when, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatalf("parse %s: %v", at, err)
		}
		return &run.Run{ID: "r", Tool: "terraform", Command: "infra", ProposedFrom: "p",
			PlanDestroys: &seven, CreatedAt: when}
	}
	if Denying(set, apply("2026-09-30T12:00:00Z")) == nil {
		t.Error("the port let a weekday apply destroying seven resources through")
	}
	if Denying(set, apply("2026-10-03T12:00:00Z")) != nil {
		t.Error("the port refused a weekend apply the Sentinel rule allows")
	}
	if !PlanGated(set, &run.Run{ID: "a", Tool: "terraform", Command: "infra"}) {
		t.Error("the port does not plan the apply it judges")
	}
}
