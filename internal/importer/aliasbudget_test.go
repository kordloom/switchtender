package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// aliasBombVars is an inventory variables string of nested aliases: padding scalars that keep the
// YAML library's alias ratio guard quiet, one anchored 64-byte string, a list of 100 aliases to it,
// and a list of 1960 aliases to that list. About 40 KB as written, it decodes to more than 12 MiB.
func aliasBombVars() string {
	var b strings.Builder
	for i := range 4000 {
		fmt.Fprintf(&b, "p%d: x\n", i)
	}
	b.WriteString("s: &s " + strings.Repeat("a", 64) + "\n")
	b.WriteString("l: &l [" + strings.TrimSuffix(strings.Repeat("*s,", 100), ",") + "]\n")
	b.WriteString("big: [" + strings.TrimSuffix(strings.Repeat("*l,", 1960), ",") + "]\n")
	return b.String()
}

// awxInventoryExport returns an AWX export of one inventory, named name, whose variables string is
// vars.
func awxInventoryExport(t *testing.T, name, vars string) []byte {
	t.Helper()
	quoted, err := json.Marshal(vars)
	if err != nil {
		t.Fatalf("quote the variables: %v", err)
	}
	return []byte(`{"inventory": [{"name": "` + name + `", "variables": ` + string(quoted) +
		`, "related": {"hosts": [{"name": "web1"}]}}]}`)
}

// TestAWXVariablesAliasExpansionBounded proves an AWX export whose variables expand through nested
// aliases past the import's budget is refused with a sentence naming the value, where it decoded to
// more than 12 MiB of inventory text from about 40 KB. A map shared by two keys through one anchor
// is ordinary YAML and still imports expanded, which is the control.
func TestAWXVariablesAliasExpansionBounded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Vars is the inventory's variables string.
		Vars string
		// Want is the error FromAWX returns.
		Want error
		// WantContent are strings the inventory content holds when the import succeeds.
		WantContent []string
	}{{ // Test 0: Nested aliases past the budget refuse the import.
		Vars: aliasBombVars(), Want: ErrExpansion,
	}, { // Test 1: One anchored map shared by two keys imports with the content expanded under both.
		Vars: "base: &b {port: 8080, tier: front}\nweb: *b\nedge: *b\n", Want: nil,
		WantContent: []string{"web=", "edge=", "8080"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			data := awxInventoryExport(t, "fleet", test.Vars)
			plan, err := FromAWX(data, testNow)
			if !errors.Is(err, test.Want) {
				size := 0
				if plan != nil && len(plan.Inventories) > 0 {
					size = len(plan.Inventories[0].Content)
				}
				t.Fatalf("FromAWX() error = %v, want %v (inventory content %d bytes from %d)", err,
					test.Want, size, len(data))
			}
			if test.Want != nil {
				if !strings.Contains(err.Error(), `inventory "fleet"`) {
					t.Errorf("FromAWX() error = %v, want it to name the inventory's variables", err)
				}
				return
			}
			if len(plan.Inventories) != 1 {
				t.Fatalf("inventories = %d, want 1 (warnings %v)", len(plan.Inventories), plan.Warnings)
			}
			content := plan.Inventories[0].Content
			for _, want := range test.WantContent {
				if !strings.Contains(content, want) {
					t.Errorf("inventory content lacks %q:\n%s", want, content)
				}
			}
		})
	}
}

// TestAWXAliasBudgetIsSharedByTheWholeExport proves the budget covers the export as a whole: many
// hosts whose variables each stay under the per-import floor on their own, and together expand far
// past it, are refused. A budget per value let each one expand to the limit.
func TestAWXAliasBudgetIsSharedByTheWholeExport(t *testing.T) {
	t.Parallel()
	// Each host's variables expand to about 64 KB from under 400 bytes.
	one := "s: &s " + strings.Repeat("a", 64) + "\nl: &l [" +
		strings.TrimSuffix(strings.Repeat("*s,", 32), ",") + "]\nbig: [" +
		strings.TrimSuffix(strings.Repeat("*l,", 32), ",") + "]\n"
	quoted, err := json.Marshal(one)
	if err != nil {
		t.Fatalf("quote the variables: %v", err)
	}
	hosts := make([]string, 400)
	for i := range hosts {
		hosts[i] = fmt.Sprintf(`{"name": "h%d", "variables": %s}`, i, quoted)
	}
	data := []byte(`{"inventory": [{"name": "fleet", "related": {"hosts": [` +
		strings.Join(hosts, ",") + `]}}]}`)
	if _, err := FromAWX(data, testNow); !errors.Is(err, ErrExpansion) {
		t.Fatalf("FromAWX() error = %v, want %v for %d hosts of nested aliases", err, ErrExpansion,
			len(hosts))
	}
}

// TestRundeckAliasExpansionBounded proves a Rundeck job whose steps each alias one command holding a
// large script is refused past the budget, rather than decoded into thousands of copies of it.
func TestRundeckAliasExpansionBounded(t *testing.T) {
	t.Parallel()
	script := strings.Repeat("echo padding\n", 320)
	var b strings.Builder
	b.WriteString("- name: Fan out\n  sequence:\n    commands:\n")
	b.WriteString("      - &c\n        script: |\n")
	for line := range strings.SplitSeq(strings.TrimSuffix(script, "\n"), "\n") {
		b.WriteString("          " + line + "\n")
	}
	for range 4000 {
		b.WriteString("      - *c\n")
	}
	_, err := FromRundeck("hosts")([]byte(b.String()), testNow)
	if !errors.Is(err, ErrExpansion) {
		t.Fatalf("FromRundeck() error = %v, want %v", err, ErrExpansion)
	}
	if !strings.Contains(err.Error(), `job "Fan out"`) {
		t.Errorf("FromRundeck() error = %v, want it to name the job", err)
	}
}

// TestTowerCLIAliasExpansionBounded proves the check for a tower-cli file charges its YAML to a
// budget before decoding it, since an asset_type of any type decodes whatever an alias names.
func TestTowerCLIAliasExpansionBounded(t *testing.T) {
	t.Parallel()
	doc := "- asset_type: &l [" + strings.TrimSuffix(strings.Repeat(`"`+strings.Repeat("a", 64)+`",`,
		100), ",") + "]\n- asset_type: [" + strings.TrimSuffix(strings.Repeat("*l,", 1960), ",") + "]\n"
	if _, err := FromAWX([]byte(doc), testNow); !errors.Is(err, ErrExpansion) {
		t.Fatalf("FromAWX() error = %v, want %v", err, ErrExpansion)
	}
}
