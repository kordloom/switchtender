package importer_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/importer"
)

// TestMergeListWarnsOnASharedKey pins that an AWX host whose variables use a merge key listing two
// maps that both set a key is imported with a warning naming the key and the value stored, since
// PyYAML releases disagree on the winner and AWX used whichever its release picked.
func TestMergeListWarnsOnASharedKey(t *testing.T) {
	t.Parallel()
	vars := "extra: &extra\n  size: large\nbase: &base\n  size: small\n  region: x\nmerged:\n  <<: [*extra, *base]\n"
	export := map[string]any{"inventory": []any{map[string]any{"name": "Fleet",
		"hosts": []any{map[string]any{"name": "w1", "variables": vars}}}}}
	data, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := importer.FromAWX(data, time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	var got string
	for _, w := range plan.Warnings {
		if strings.Contains(w, "merge key") {
			got = w
		}
	}
	for _, want := range []string{`host "w1"`, `both set "size"`, `Set "size" on the host itself`,
		`stored "large"`} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q does not hold %q (warnings %q)", got, want, plan.Warnings)
		}
	}
	if strings.Contains(got, "region") {
		t.Errorf("warning %q names region, which only one map sets", got)
	}
}
