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
