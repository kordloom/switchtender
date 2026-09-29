package run

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestAssessRiskNeverDowngrades pins that a run carrying several elevated signals grades at the
// highest of them, not at whichever one the function looked at last.
//
// The grade is what a min_risk approval rule compares against. A destructive Terraform run that
// graded medium because the infrastructure-apply signal ran after the destructive-command one would
// pass a rule set to hold only high-risk changes, and the run that slipped through would be the
// worst one on the install.
func TestAssessRiskNeverDowngrades(t *testing.T) {
	t.Parallel()
	shards := func(n int) *int { return &n }
	tests := []struct {
		Name      string
		Run       *Run
		WantLevel string
	}{{ // Test 0: A destructive Terraform run keeps the high grade the marker set.
		Name:      "destroy and infra apply",
		Run:       &Run{Tool: ToolTerraform, Command: "terraform destroy -auto-approve"},
		WantLevel: RiskHigh,
	}, { // Test 1: A destructive Ansible run against the whole fleet stays high.
		Name:      "destructive and unlimited",
		Run:       &Run{Tool: ToolAnsible, Playbook: "wipe.yml", Command: "rm -rf /data"},
		WantLevel: RiskHigh,
	}, { // Test 2: A destructive run in a wide split stays high.
		Name: "destructive and wide fan out",
		Run: &Run{Tool: ToolBash, Command: "mkfs /dev/sdb", Limit: "web01",
			ShardCount: shards(200)},
		WantLevel: RiskHigh,
	}, { // Test 3: Two medium signals together are still medium, not high by accumulation.
		Name:      "infra apply in a wide split",
		Run:       &Run{Tool: ToolOpenTofu, Command: "/infra", ShardCount: shards(80)},
		WantLevel: RiskMedium,
	}, { // Test 4: A dry run outranks everything, because it changes nothing.
		Name: "dry run of a destroy across the fleet",
		Run: &Run{Tool: ToolTerraform, Command: "terraform destroy", DryRun: true,
			ShardCount: shards(500)},
		WantLevel: RiskLow,
	}, { // Test 5: A shard count one below the fan-out threshold is not wide by itself.
		Name:      "just under the fan out threshold",
		Run:       &Run{Tool: ToolBash, Command: "echo hi", ShardCount: shards(49)},
		WantLevel: RiskLow,
	}, { // Test 6: Exactly at the threshold is wide.
		Name:      "exactly at the fan out threshold",
		Run:       &Run{Tool: ToolBash, Command: "echo hi", ShardCount: shards(50)},
		WantLevel: RiskMedium,
	}, { // Test 7: A non-Ansible tool with no limit is not graded on inventory breadth, since the
		// limit means nothing to it.
		Name:      "bash with no limit",
		Run:       &Run{Tool: ToolBash, Command: "echo hi"},
		WantLevel: RiskLow,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := AssessRisk(test.Run)
			if got.Level != test.WantLevel {
				t.Errorf("AssessRisk() = %q, want %q (reasons %v)",
					got.Level, test.WantLevel, got.Reasons)
			}
			if len(got.Reasons) == 0 {
				t.Error("the grade carries no reason, so an approver is shown a level with no basis")
			}
		})
	}
}

// TestAssessRiskOfNothingIsLow pins that grading a run that is not there answers low rather than
// panicking, since the risk is computed on read and a caller may hold nothing.
func TestAssessRiskOfNothingIsLow(t *testing.T) {
	t.Parallel()
	got := AssessRisk(nil)
	if got.Level != RiskLow {
		t.Errorf("AssessRisk(nil) = %q, want low", got.Level)
	}
}

// TestRaiseOrdersTheThreeLevels pins the level ordering the grader raises through, including that
// an unknown level does not outrank a known one.
func TestRaiseOrdersTheThreeLevels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Cur       string
		Next      string
		WantLevel string
	}{
		{Cur: RiskLow, Next: RiskMedium, WantLevel: RiskMedium},    // Test 0: Up one.
		{Cur: RiskLow, Next: RiskHigh, WantLevel: RiskHigh},        // Test 1: Up two.
		{Cur: RiskMedium, Next: RiskHigh, WantLevel: RiskHigh},     // Test 2: Up one from the middle.
		{Cur: RiskHigh, Next: RiskMedium, WantLevel: RiskHigh},     // Test 3: Never down.
		{Cur: RiskHigh, Next: RiskLow, WantLevel: RiskHigh},        // Test 4: Never down two.
		{Cur: RiskMedium, Next: RiskLow, WantLevel: RiskMedium},    // Test 5: Never down.
		{Cur: RiskMedium, Next: RiskMedium, WantLevel: RiskMedium}, // Test 6: Equal stays.
		{Cur: RiskHigh, Next: "unknown", WantLevel: RiskHigh},      // Test 7: An unknown level ranks
		// as zero and cannot pull a graded run down.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := raise(test.Cur, test.Next); got != test.WantLevel {
				t.Errorf("raise(%q, %q) = %q, want %q", test.Cur, test.Next, got, test.WantLevel)
			}
		})
	}
}

// TestWholeInventoryLimitReadsAnAwkwardPattern pins the breadth test against patterns that are
// legal Ansible but are not what the readable cases look like.
//
// The test decides whether a mutating run is graded on its blast radius, so a pattern it misreads
// as narrow takes a fleet-wide change out of every rule keyed on a minimum risk. Erring wide is the
// stated direction, so the doubtful shapes must come back wide.
func TestWholeInventoryLimitReadsAnAwkwardPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Limit    string
		WantWide bool
	}{
		{Name: "whitespace only", Limit: "   ", WantWide: true},       // Test 0: Same as empty.
		{Name: "tab only", Limit: "\t", WantWide: true},               // Test 1: Same.
		{Name: "newline only", Limit: "\n", WantWide: true},           // Test 2: Same.
		{Name: "separators only", Limit: ":,:", WantWide: true},       // Test 3: Names nothing.
		{Name: "padded all", Limit: "  all  ", WantWide: true},        // Test 4: Trimmed.
		{Name: "mixed case all", Limit: "AlL", WantWide: true},        // Test 5: Folded.
		{Name: "all then a group", Limit: "all:web", WantWide: false}, // Test 6: A real term
		// narrows the pattern, so it is not the whole inventory any more.
		{Name: "empty term between reals", Limit: "web::db", WantWide: false}, // Test 7: Narrow.
		{Name: "star with an exclusion", Limit: "*:!db", WantWide: true},      // Test 8: Wide.
		{Name: "several exclusions only", Limit: "!web,!db", WantWide: true},  // Test 9: Still
		// starts from everything.
		{Name: "an intersection only", Limit: "&web", WantWide: true}, // Test 10: Intersections do
		// not select on their own, so nothing narrows the start.
		{Name: "unicode group", Limit: "配置", WantWide: false},         // Test 11: A real name.
		{Name: "a host named allow", Limit: "allow", WantWide: false}, // Test 12: The match is the
		// whole term, not a prefix of it.
		{Name: "a host named all01", Limit: "all01", WantWide: false}, // Test 13: Same.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := WholeInventoryLimit(test.Limit); got != test.WantWide {
				t.Errorf("WholeInventoryLimit(%q) = %v, want %v", test.Limit, got, test.WantWide)
			}
		})
	}
}

// TestAssessRiskReadsDestructiveTextInsideNestedVariables pins that a destructive command hidden in
// a list or a nested object grades the same as one in a plain string variable.
//
// A variable is string material a playbook or script splices into what it executes, which is why
// string variables are already scanned. Variables are not only strings: -e '{"cmds":["rm -rf /"]}'
// and -e '{"job":{"cmd":"rm -rf /"}}' are both ordinary JSON bodies against the same API, and both
// end up on a command line the same way. Graded low, they pass every rule set to hold a high-risk
// change, and the fact that the same text typed on the command line is caught is what makes this a
// gap rather than a design choice.
func TestAssessRiskReadsDestructiveTextInsideNestedVariables(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Vars map[string]any
	}{{ // Test 0: A list of commands, which is how a loop variable is written.
		Name: "list of strings", Vars: map[string]any{"cmds": []any{"rm -rf /data"}},
	}, { // Test 1: A nested object, which is how a structured job variable is written.
		Name: "nested map", Vars: map[string]any{"job": map[string]any{"cmd": "rm -rf /data"}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			// The limit narrows the run so nothing but the variable can raise the grade.
			got := AssessRisk(&Run{Playbook: "maintenance.yml", Limit: "web01",
				ExtraVars: test.Vars})
			if got.Level != RiskHigh {
				t.Errorf("AssessRisk() = %q, want high: the destructive text is in %s (reasons %v)",
					got.Level, test.Name, got.Reasons)
			}
		})
	}
}

// TestValidNotifyKindRefusesAnythingItCannotFormat pins that the kind check names exactly the
// channels there is a formatter for.
//
// A kind that passed the check but had no formatter would be stored on a template and then reach
// nobody every time a run finished, which is a page that never arrives rather than an error anybody
// sees.
func TestValidNotifyKindRefusesAnythingItCannotFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Kind      string
		WantValid bool
	}{
		{Kind: NotifyWebhook, WantValid: true},     // Test 0: Configured by a URL.
		{Kind: NotifySlack, WantValid: true},       // Test 1: Same.
		{Kind: NotifyMattermost, WantValid: true},  // Test 2: Same.
		{Kind: NotifyRocketChat, WantValid: true},  // Test 3: Same.
		{Kind: NotifyDiscord, WantValid: true},     // Test 4: Same.
		{Kind: NotifyTeams, WantValid: true},       // Test 5: Same.
		{Kind: NotifyNtfy, WantValid: true},        // Test 6: Same.
		{Kind: NotifyPagerDuty, WantValid: true},   // Test 7: Carries its own routing key.
		{Kind: NotifyGrafana, WantValid: true},     // Test 8: Carries a URL and a token.
		{Kind: NotifyTwilio, WantValid: true},      // Test 9: Names a recipient only.
		{Kind: NotifyEmail, WantValid: true},       // Test 10: Same.
		{Kind: "", WantValid: false},               // Test 11: Empty names no channel.
		{Kind: "SLACK", WantValid: false},          // Test 12: The match is exact, not folded.
		{Kind: " slack", WantValid: false},         // Test 13: A stray space is not a channel.
		{Kind: "carrier-pigeon", WantValid: false}, // Test 14: Nothing formats this.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %q", testNum, test.Kind), func(t *testing.T) {
			t.Parallel()
			if got := ValidNotifyKind(test.Kind); got != test.WantValid {
				t.Errorf("ValidNotifyKind(%q) = %v, want %v", test.Kind, got, test.WantValid)
			}
			// Every kind the check accepts is one the per-target validator can rule on, so a kind
			// cannot pass one gate and fall through the other.
			err := ValidateNotifyTarget(NotifyTarget{
				Kind: test.Kind, URL: "https://h", Key: "k", To: "a@b.co",
			})
			if test.WantValid != (err == nil) {
				t.Errorf("ValidateNotifyTarget(%q) error = %v, disagreeing with ValidNotifyKind",
					test.Kind, err)
			}
		})
	}
}

// TestNormalizeAndValidTool pins the empty-means-Ansible rule and the refusal of anything else.
//
// The tool decides which runner executes the run, so an unknown name reaching the executor is a
// dispatch with nothing to dispatch to. Empty has always meant Ansible, and runs stored before the
// field existed carry it, so treating empty as invalid would strand every one of them.
func TestNormalizeAndValidTool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In        string
		WantTool  string
		WantValid bool
	}{
		{In: "", WantTool: ToolAnsible, WantValid: true},                 // Test 0: The historical form.
		{In: ToolAnsible, WantTool: ToolAnsible, WantValid: true},        // Test 1: Named.
		{In: ToolBash, WantTool: ToolBash, WantValid: true},              // Test 2: Bash.
		{In: ToolTerraform, WantTool: ToolTerraform, WantValid: true},    // Test 3: Terraform.
		{In: ToolOpenTofu, WantTool: ToolOpenTofu, WantValid: true},      // Test 4: OpenTofu.
		{In: ToolPython, WantTool: ToolPython, WantValid: true},          // Test 5: Python.
		{In: ToolPowerShell, WantTool: ToolPowerShell, WantValid: true},  // Test 6: PowerShell.
		{In: ToolGo, WantTool: ToolGo, WantValid: true},                  // Test 7: Go.
		{In: "ANSIBLE", WantTool: "ANSIBLE", WantValid: false},           // Test 8: Exact, not folded.
		{In: " bash", WantTool: " bash", WantValid: false},               // Test 9: Not trimmed.
		{In: "bash ", WantTool: "bash ", WantValid: false},               // Test 10: Same.
		{In: "sh", WantTool: "sh", WantValid: false},                     // Test 11: Not a tool.
		{In: "../../bin/sh", WantTool: "../../bin/sh", WantValid: false}, // Test 12: A path is not
		// a tool name, so a caller cannot name a binary.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %q", testNum, test.In), func(t *testing.T) {
			t.Parallel()
			if got := NormalizeTool(test.In); got != test.WantTool {
				t.Errorf("NormalizeTool(%q) = %q, want %q", test.In, got, test.WantTool)
			}
			if got := ValidTool(test.In); got != test.WantValid {
				t.Errorf("ValidTool(%q) = %v, want %v", test.In, got, test.WantValid)
			}
		})
	}
}

// TestExtraToolNamesIsASortedCopy pins that the registered extension names come back sorted and
// that a caller editing the answer cannot change the registry, since the registry is written only
// at startup and read without a lock while serving.
func TestExtraToolNamesIsASortedCopy(t *testing.T) {
	t.Parallel()
	first := ExtraToolNames()
	if len(first) > 0 {
		first[0] = "\x00mutated"
	}
	second := ExtraToolNames()
	for _, name := range second {
		if name == "\x00mutated" {
			t.Fatal("editing the returned slice changed the tool registry")
		}
	}
	for i := 1; i < len(second); i++ {
		if second[i-1] > second[i] {
			t.Errorf("ExtraToolNames() is not sorted: %v", second)
			break
		}
	}
	if diff := cmp.Diff(len(first), len(second), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the registry changed size between two reads (-want +got):\n%s", diff)
	}
}
