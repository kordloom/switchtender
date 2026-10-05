package outcome

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// TestBodyCommitsWhatADryRunForced pins the signed outcome of a dry run the gate scanned. The
// record already says dry_run, and without the scan beside it a receipt for a run that changed
// hosts, or ran a program while it planned, read exactly like one for a preview that changed
// nothing. The record carries the scanner and its version, the files examined, what was found, and
// the classification. A run the gate never scanned carries no key at all, so its record reduces to
// the bytes it always did.
func TestBodyCommitsWhatADryRunForced(t *testing.T) {
	t.Parallel()
	forced := run.DryRunScan{Tool: run.ToolAnsible, Scanner: "ansible-check-mode", Version: 1,
		Inputs:   []string{"site.yml"},
		Findings: []string{`site.yml: task "Restart web" sets check_mode to false`}}.Classified()
	program := run.DryRunScan{Tool: run.ToolTerraform, Scanner: "terraform-external", Version: 1,
		Source: "read at commit 0123456789ab, the project's last synced commit",
		Inputs: []string{"infra/main.tf", "infra/modules/net/main.tf"},
		Findings: []string{"module.net.data.external.lookup runs a program during plan " +
			"(infra/modules/net/main.tf line 1)"}}.Classified()
	tests := []struct {
		Scans    []run.DryRunScan
		WantJSON []string
		WantKey  bool
	}{{ // Test 0: A dry run that forced a task commits the scan.
		Scans: []run.DryRunScan{forced},
		WantJSON: []string{`"scanner":"ansible-check-mode","version":1`, `"inputs":["site.yml"]`,
			`"findings":["site.yml: task \"Restart web\" sets check_mode to false"]`,
			`"classification":"not_change_free"`},
		WantKey: true,
	}, { // Test 1: A plan that runs a program commits the address, the files read, and the commit.
		Scans: []run.DryRunScan{program},
		WantJSON: []string{`"tool":"terraform","scanner":"terraform-external","version":1`,
			`"source":"read at commit 0123456789ab, the project's last synced commit"`,
			`"inputs":["infra/main.tf","infra/modules/net/main.tf"]`,
			`module.net.data.external.lookup runs a program during plan`,
			`"classification":"not_change_free"`},
		WantKey: true,
	}, { // Test 2: A run the gate never scanned commits no key.
		Scans: nil, WantKey: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded,
				ExitCode: intPtr(0), DryRun: true, DryRunScans: test.Scans}
			body, err := Body(context.Background(), &fakeRunStore{}, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			if got := strings.Contains(string(body), `"dry_run_scans"`); got != test.WantKey {
				t.Fatalf("body = %s, carries dry_run_scans = %v, want %v", body, got, test.WantKey)
			}
			for _, want := range test.WantJSON {
				if !strings.Contains(string(body), want) {
					t.Errorf("body = %s, want it to carry %s", body, want)
				}
			}
			rec, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if diff := cmp.Diff(test.Scans, rec.DryRunScans, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("parsed scans mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
