package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAPluginToolsPlanIsNotChangeFree holds the pull request pre-check to what it can read. A plan
// nothing scanned passes only as a built-in tool's inert check: a tool a plugin or the SDK added runs
// whatever the plugin coded, so its plan is refused, and the refusal names the tool and says the gate
// cannot read it. A built-in tool with no scan, and a clean scan, still pass, and a scan with a
// finding is refused as before.
func TestAPluginToolsPlanIsNotChangeFree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Scans are what the gate read of the plan.
		Scans []run.DryRunScan
		// Tool is the plan's tool.
		Tool string
		// WantChangeFree is whether the plan passes the pre-check.
		WantChangeFree bool
		// WantReason is the refusal reason when it does not pass.
		WantReason string
	}{{ // Test 0: A plugin tool's plan, which nothing scans, is refused.
		Tool: "acme-deploy", WantReason: "a plugin tool's plan cannot be read",
	}, { // Test 1: A built-in tool's inert check with no scan passes.
		Tool: run.ToolBash, WantChangeFree: true,
	}, { // Test 2: A clean scan passes.
		Scans: []run.DryRunScan{{Tool: run.ToolTerraform}}, Tool: run.ToolTerraform,
		WantChangeFree: true,
	}, { // Test 3: A scan with a finding is refused, the control.
		Scans: []run.DryRunScan{{Tool: run.ToolTerraform,
			Findings: []string{"data.external.x runs a program during plan (main.tf line 1)"}}},
		Tool: run.ToolTerraform, WantReason: "the configuration runs a program while it plans",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := planChangeFree(test.Scans, test.Tool); got != test.WantChangeFree {
				t.Fatalf("planChangeFree() = %v, want %v", got, test.WantChangeFree)
			}
			if test.WantChangeFree {
				return
			}
			message, reason := dryRunRefusal(test.Scans, test.Tool)
			if reason != test.WantReason {
				t.Errorf("reason = %q, want %q", reason, test.WantReason)
			}
			if len(test.Scans) == 0 && !strings.Contains(message, test.Tool) {
				t.Errorf("message %q does not name the tool %q", message, test.Tool)
			}
		})
	}
}
