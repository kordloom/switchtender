package policy_test

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/policytest"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRegoInputCarriesEveryDocumentedField holds the schema the load-time check uses to the
// document evaluation builds, in both directions, so a field cannot be added to one and not the
// other. It also pins that a run with nothing set still yields every field.
func TestRegoInputCarriesEveryDocumentedField(t *testing.T) {
	t.Parallel()
	got := schemaPaths("input", anyShape(policy.RegoInput(&run.Run{})))
	want := schemaPaths("input", policy.RegoInputSchema)
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("input document and schema disagree (-schema +document):\n%s", diff)
	}
	in := policy.RegoInput(&run.Run{Tool: "", PlanDestroys: nil})
	if tool := in["run"].(map[string]any)["tool"]; tool != run.ToolAnsible {
		t.Errorf("run.tool = %v, want the normalized tool", tool)
	}
	plan := in["plan"].(map[string]any)
	if plan["planned"] != false || plan["destroys"] != nil {
		t.Errorf("plan = %v, want unplanned with no destroy count", plan)
	}
	if in["version"] != policy.RegoInputVersion {
		t.Errorf("version = %v, want %d", in["version"], policy.RegoInputVersion)
	}
}

// TestThePolicyPageSampleInputIsTheDocument decodes the sample input the policy page shows, so the
// page cannot show an input that is not the document a policy is evaluated against. That the page's
// modules compile is checked by the paid build, which is the build that compiles them.
func TestThePolicyPageSampleInputIsTheDocument(t *testing.T) {
	t.Parallel()
	var inputs int
	for _, block := range policytest.DocBlocks(t, filepath.Join("..", "..", "docs", "policy.md")) {
		if !strings.HasPrefix(block, "{") {
			continue
		}
		inputs++
		var doc map[string]any
		if err := json.Unmarshal([]byte(block), &doc); err != nil {
			t.Fatalf("the sample input is not JSON: %v", err)
		}
		got := schemaPaths("input", anyShape(doc))
		want := schemaPaths("input", anyShape(policy.RegoInput(&run.Run{})))
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("the sample input is not the input document (-document +sample):\n%s", diff)
		}
	}
	if inputs != 1 {
		t.Errorf("found %d inputs on the page, want exactly 1", inputs)
	}
}

// anyShape converts a document to the schema's vocabulary: objects stay objects, everything else is
// a leaf, and labels are an open object.
func anyShape(doc map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range doc {
		if k == "labels" {
			out[k] = policy.RegoAnyKeys
			continue
		}
		if m, ok := v.(map[string]any); ok {
			out[k] = anyShape(m)
			continue
		}
		out[k] = nil
	}
	return out
}

// schemaPaths lists every path in a schema-shaped document, sorted.
func schemaPaths(prefix string, node map[string]any) []string {
	var out []string
	for k, v := range node {
		path := prefix + "." + k
		out = append(out, path)
		if m, ok := v.(map[string]any); ok {
			out = append(out, schemaPaths(path, m)...)
		}
	}
	sort.Strings(out)
	return out
}
