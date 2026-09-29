package run

import "strings"

// Risk grades how dangerous a run is so an approver weighs a hold with the blast radius in view,
// rather than rubber-stamping it. It is computed from the run, never stored.
type Risk struct {
	// Level is low, medium, or high.
	Level string `json:"level"`
	// Reasons lists the signals that set the level, most to least severe.
	Reasons []string `json:"reasons,omitempty"`
}

// Risk levels, ordered.
const (
	// RiskLow is a run with no elevated signal.
	RiskLow = "low"
	// RiskMedium is a run that changes state but carries no destructive signal.
	RiskMedium = "medium"
	// RiskHigh is a run that destroys, targets broadly, or forces past safety checks.
	RiskHigh = "high"
)

// destructiveMarkers are command fragments that signal a run can delete or disrupt, matched case
// insensitively against the run's command. They are advisory signals for an approver, not a policy.
var destructiveMarkers = []string{
	"terraform destroy", "tofu destroy", "destroy -", "rm -rf", "rm -fr", "mkfs", "dd if=",
	"drop table", "drop database", "truncate ", "reboot", "shutdown", "halt", "--force", "-force",
	"del /f", "remove-item", "format-volume",
}

// AssessRisk grades r from its tool, command, and blast radius. A dry run is always low since it
// changes nothing. A destructive marker or a Terraform apply that is not a dry run is high; a state
// change with a wide target is medium. The result carries the reasons so an approver sees why.
func AssessRisk(r *Run) Risk {
	if r == nil {
		return Risk{Level: RiskLow}
	}
	if r.DryRun {
		return Risk{Level: RiskLow, Reasons: []string{"dry run, makes no changes"}}
	}

	var reasons []string
	level := RiskLow

	lower := strings.ToLower(r.Command + " " + r.Playbook)
	for _, m := range destructiveMarkers {
		if strings.Contains(lower, m) {
			reasons = append(reasons, "destructive command: "+strings.TrimSpace(m))
			level = RiskHigh
			break
		}
	}

	switch NormalizeTool(r.Tool) {
	case ToolTerraform, ToolOpenTofu:
		reasons = append(reasons, "infrastructure apply")
		level = raise(level, RiskMedium)
	}

	// A wide target multiplies the blast radius: a mutating run against every host, or a large fan
	// out, is at least medium.
	if r.Limit == "" && NormalizeTool(r.Tool) == ToolAnsible {
		reasons = append(reasons, "targets the whole inventory, no limit")
		level = raise(level, RiskMedium)
	}
	if r.ShardCount != nil && *r.ShardCount >= 50 {
		reasons = append(reasons, "fans out across many hosts")
		level = raise(level, RiskMedium)
	}

	if level == RiskLow {
		reasons = append(reasons, "no elevated signal")
	}
	return Risk{Level: level, Reasons: reasons}
}

// raise returns the higher of two risk levels.
func raise(cur, next string) string {
	rank := map[string]int{RiskLow: 0, RiskMedium: 1, RiskHigh: 2}
	if rank[next] > rank[cur] {
		return next
	}
	return cur
}
