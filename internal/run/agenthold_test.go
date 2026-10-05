package run

import (
	"fmt"
	"strings"
	"testing"
)

// TestAgentHoldNotes holds the two notes the built-in agent hold writes to saying what was found
// and offering an exemption as the second fix, and to saying nothing when nothing was found.
func TestAgentHoldNotes(t *testing.T) {
	t.Parallel()
	external := DryRunScan{Tool: ToolTerraform,
		Findings: []string{"data.external.x runs a program during plan (main.tf line 1)"}}
	forced := DryRunScan{Tool: ToolAnsible,
		Findings: []string{`site.yml: task "Restart web" sets check_mode to false`}}
	unread := DryRunScan{Tool: ToolOpenTofu,
		Unread: []string{`module.vpc from "acme/vpc/aws" (not downloaded here)`}}
	clean := DryRunScan{Tool: ToolTerraform, Inputs: []string{"main.tf"}}
	const (
		planLead = "Planning this apply was not shown to change nothing, so the default hold " +
			"on an agent's run applies before it plans: "
		dryLead = "This dry run was not shown to change nothing, so the default hold on an " +
			"agent's run applies: "
		exemptFix = "or write a policy with effect exempt that covers this run."
	)
	tests := []struct {
		// Note builds the note under test.
		Note func() string
		// WantParts are what the note must say, in order, empty for no note at all.
		WantParts []string
	}{{ // Test 0: An apply whose plan read found nothing has no note.
		Note: func() string { return AgentPlanHoldNote(nil) },
	}, { // Test 1: A plan read in full that found nothing has none either.
		Note: func() string { return AgentPlanHoldNote([]DryRunScan{clean}) },
	}, { // Test 2: An external data source is named, with both fixes.
		Note: func() string { return AgentPlanHoldNote([]DryRunScan{external}) },
		WantParts: []string{planLead, "data.external.x runs a program during plan",
			"replace the external data source", exemptFix},
	}, { // Test 3: A configuration not read in full names what was not read.
		Note: func() string { return AgentPlanHoldNote([]DryRunScan{unread}) },
		WantParts: []string{planLead, `module.vpc from "acme/vpc/aws"`,
			"make every module the configuration calls readable", exemptFix},
	}, { // Test 4: A forcing dry run names the task, with both fixes.
		Note: func() string {
			return AgentHoldNote(&Run{DryRun: true, DryRunScans: []DryRunScan{forced}})
		},
		WantParts: []string{dryLead, `task "Restart web" sets check_mode to false`,
			"rework the task so check mode is safe", exemptFix},
	}, { // Test 5: A dry run whose scans found nothing has no note.
		Note: func() string { return AgentHoldNote(&Run{DryRun: true}) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			note := test.Note()
			if len(test.WantParts) == 0 {
				if note != "" {
					t.Errorf("note = %q, want none", note)
				}
				return
			}
			rest := note
			for _, want := range test.WantParts {
				i := strings.Index(rest, want)
				if i < 0 {
					t.Fatalf("note %q does not say %q in order", note, want)
				}
				rest = rest[i+len(want):]
			}
			if !strings.HasPrefix(note, test.WantParts[0]) {
				t.Errorf("note %q does not lead with %q", note, test.WantParts[0])
			}
		})
	}
}
