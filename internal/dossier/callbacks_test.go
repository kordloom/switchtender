package dossier

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRunMetaStatesTheFactCache pins that the evidence states the fact cache setting an approval
// binds to: a run that served cached facts says so with its timeout, in the words the approval view
// uses, and a run that did not says nothing about it.
func TestRunMetaStatesTheFactCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantRow  string
		UseCache bool
		Timeout  int
	}{{ // Test 0: The cache with a timeout names it.
		UseCache: true, Timeout: 3600, WantRow: "uses cached facts (timeout 3600s)",
	}, { // Test 1: The cache with no timeout says facts of any age were served.
		UseCache: true, WantRow: "uses cached facts (no timeout)",
	}, { // Test 2: A run without the cache has no row, even with a timeout left on it.
		Timeout: 3600,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rows := runMeta(&run.Run{ID: "run_facts", Playbook: "site.yml", CreatedAt: evidenceTime,
				UseFactCache: test.UseCache, FactCacheTimeout: test.Timeout})
			got := ""
			for _, row := range rows {
				if row.K == "Fact cache" {
					got = row.V
				}
			}
			switch {
			case test.WantRow == "" && got != "":
				t.Errorf("Fact cache = %q, want no row for a run that did not use the cache", got)
			case test.WantRow != "" && !strings.HasPrefix(got, test.WantRow):
				t.Errorf("Fact cache = %q, want it to begin %q", got, test.WantRow)
			}
		})
	}
}

// TestDossierSaysWhichCallbackAddressAHostUsed pins decision twenty-one's evidence: a run a host
// launched through the AWX-compatible callback address says so beside its source, read from the
// chain entry that recorded the launch, and every other run says nothing about it.
func TestDossierSaysWhichCallbackAddressAHostUsed(t *testing.T) {
	t.Parallel()
	rows := []metaRow{{K: "Run", V: "run_cb"}, {K: "Source", V: "callback tpl_cb"},
		{K: "Created", V: "2026-10-01T09:00:00Z"}}
	tests := []struct {
		Launch   *audit.Entry
		WantKeys []string
		WantRow  string
	}{{ // Test 0: The AWX-compatible address is named after the source.
		Launch:   &audit.Entry{Path: "/v1/templates/tpl_cb/callback/fired/awx/42"},
		WantKeys: []string{"Run", "Source", "Called back through", "Created"},
		WantRow:  "the AWX-compatible address /api/v2/job_templates/42/callback/",
	}, { // Test 1: The template's own address adds nothing.
		Launch:   &audit.Entry{Path: "/v1/templates/tpl_cb/callback/fired"},
		WantKeys: []string{"Run", "Source", "Created"},
	}, { // Test 2: A run with no launch entry adds nothing.
		WantKeys: []string{"Run", "Source", "Created"},
	}, { // Test 3: A path that only looks like one adds nothing.
		Launch:   &audit.Entry{Path: "/v1/templates/tpl_cb/callback/fired/awx/x"},
		WantKeys: []string{"Run", "Source", "Created"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := withAWXArrival(append([]metaRow(nil), rows...), test.Launch)
			var keys []string
			row := ""
			for _, r := range got {
				keys = append(keys, r.K)
				if r.K == "Called back through" {
					row = r.V
				}
			}
			if diff := cmp.Diff(test.WantKeys, keys); diff != "" {
				t.Errorf("rows mismatch (-want +got):\n%s", diff)
			}
			if row != test.WantRow {
				t.Errorf("Called back through = %q, want %q", row, test.WantRow)
			}
		})
	}

	// The row reaches the rendered document, not only the helper.
	page, err := Render(&Input{Run: &run.Run{ID: "run_cb", Source: "callback", SourceID: "tpl_cb",
		CreatedAt: evidenceTime}, ChainOK: true,
		Launch: &audit.Entry{Seq: 3, Path: "/v1/templates/tpl_cb/callback/fired/awx/42",
			At: evidenceTime}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(page), "/api/v2/job_templates/42/callback/") {
		t.Error("the rendered dossier does not name the AWX-compatible address the host used")
	}
}
