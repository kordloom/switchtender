package run

import (
	"fmt"
	"testing"
)

// TestAssessRisk checks the blast-radius grading across dry runs, destructive commands, infra
// applies, and wide targets.
func TestAssessRisk(t *testing.T) {
	t.Parallel()
	shards := func(n int) *int { return &n }
	tests := []struct {
		Name string
		Run  *Run
		Want string
	}{{ // Test 0: A dry run is always low, whatever it would do.
		Name: "dry run destroy",
		Run:  &Run{Tool: "terraform", Command: "destroy", DryRun: true},
		Want: RiskLow,
	}, { // Test 1: A plain bash echo is low.
		Name: "harmless bash",
		Run:  &Run{Tool: "bash", Command: "echo hello"},
		Want: RiskLow,
	}, { // Test 2: A destructive command is high.
		Name: "rm -rf",
		Run:  &Run{Tool: "bash", Command: "rm -rf /var/lib/thing"},
		Want: RiskHigh,
	}, { // Test 3: A real terraform destroy is high.
		Name: "terraform destroy",
		Run:  &Run{Tool: "terraform", Command: "terraform destroy -auto-approve"},
		Want: RiskHigh,
	}, { // Test 4: A terraform apply that is not a dry run is at least medium.
		Name: "terraform apply",
		Run:  &Run{Tool: "opentofu", Command: "apply"},
		Want: RiskMedium,
	}, { // Test 5: An Ansible run with no limit touches the whole inventory, medium.
		Name: "ansible whole inventory",
		Run:  &Run{Tool: "ansible", Playbook: "site.yml"},
		Want: RiskMedium,
	}, { // Test 6: A limited Ansible run is low.
		Name: "ansible one host",
		Run:  &Run{Tool: "ansible", Playbook: "site.yml", Limit: "web01"},
		Want: RiskLow,
	}, { // Test 7: A wide fan out is medium.
		Name: "big split",
		Run:  &Run{Tool: "ansible", Playbook: "site.yml", Limit: "web*", ShardCount: shards(64)},
		Want: RiskMedium,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := AssessRisk(test.Run)
			if got.Level != test.Want {
				t.Errorf("AssessRisk() level = %q, want %q (reasons %v)", got.Level, test.Want, got.Reasons)
			}
			if len(got.Reasons) == 0 {
				t.Error("AssessRisk() returned no reasons, want at least one")
			}
		})
	}
}

// TestRiskGradesAWholeInventoryLimitAsWide pins that writing the target out does not grade a run down.
//
// The blast radius test read whether a limit was set, not what it selected, so "--limit all" -- the
// same instruction as no limit at all -- came back low risk with "no elevated signal" and slipped
// under every policy keyed on a minimum risk. Naming the target explicitly is an ordinary habit, so
// the widest run a person can ask for was the one least likely to be held.
func TestRiskGradesAWholeInventoryLimitAsWide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Limit string
		Wide  bool
	}{
		{Name: "no limit", Limit: "", Wide: true},
		{Name: "the word all", Limit: "all", Wide: true},
		{Name: "the star form", Limit: "*", Wide: true},
		{Name: "colon separated all", Limit: "all:all", Wide: true},
		{Name: "comma separated all", Limit: "all,all", Wide: true},
		{Name: "star pair", Limit: "*:*", Wide: true},
		{Name: "all with an exclusion", Limit: "all:!nogroup", Wide: true},
		{Name: "only an exclusion still starts from everything", Limit: "!web", Wide: true},
		{Name: "uppercase", Limit: "ALL", Wide: true},
		{Name: "a real group narrows", Limit: "web", Wide: false},
		{Name: "several real groups narrow", Limit: "web:db", Wide: false},
		{Name: "a pattern narrows", Limit: "web-*", Wide: false},
		{Name: "all beside a real group narrows to that group", Limit: "all:&web", Wide: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := AssessRisk(&Run{Tool: ToolAnsible, Playbook: "site.yml", Limit: test.Limit})
			wide := got.Level != RiskLow
			if wide != test.Wide {
				t.Errorf("AssessRisk(limit=%q) level = %q (wide=%v), want wide=%v; reasons %v",
					test.Limit, got.Level, wide, test.Wide, got.Reasons)
			}
		})
	}
}
