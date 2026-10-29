package policy_test

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestInForceDigestCoversTheQueueScope pins that changing which segment of the estate a rule covers
// is visible in the record of the rules in force.
//
// The queue decides where a run executes, so it is the criterion a rule uses to say "hold anything
// headed for production". Moving a rule from the production queue to a staging one turns a
// production gate off without deleting anything, and the canonical form the digest is taken over
// does not include the queue, so the record says the rule set did not change. That is the exact
// state this record exists to make visible: an operator who narrows a gate, runs a change through,
// and puts it back leaves two runs whose recorded rule sets are identical.
func TestInForceDigestCoversTheQueueScope(t *testing.T) {
	t.Parallel()
	prod := &policy.Policy{
		ID: "pol_1", Name: "prod gate", Queue: "prod", MaxDestroy: policy.DisabledMaxDestroy,
	}
	staging := *prod
	staging.Queue = "staging"
	none := *prod
	none.Queue = ""

	base := policy.InForce([]*policy.Policy{prod})
	if got := policy.InForce([]*policy.Policy{&staging}); got.Digest == base.Digest {
		t.Error("repointing a rule from the prod queue to staging left the digest unchanged, so a " +
			"production gate can be turned off and back on with no trace in the record")
	}
	if got := policy.InForce([]*policy.Policy{&none}); got.Digest == base.Digest {
		t.Error("widening a rule from one queue to every queue left the digest unchanged")
	}
}

// TestCommandCriteriaSeeOnlyTheCommandLine pins the boundary of what a command criterion looks at,
// which is worth knowing exactly because the risk grader looks at more.
//
// The criterion reads the command and nothing else, as documented. The playbook name and the extra
// variables are not searched, so a rule keyed on a command fragment does not reach the same text
// carried in a variable, while the risk grader does reach it and grades the run high for exactly
// that reason. A rule meant to catch destructive text has to use the risk floor, or name the tool,
// rather than relying on the command criterion alone.
func TestCommandCriteriaSeeOnlyTheCommandLine(t *testing.T) {
	t.Parallel()
	rule := &policy.Policy{
		ID: "pol_1", Name: "no drops", CommandContains: "drop database",
		MaxDestroy: policy.DisabledMaxDestroy,
	}
	onCommandLine := &run.Run{Tool: run.ToolBash, Command: "psql -c 'drop database prod'"}
	if !rule.MatchesPlain(onCommandLine) {
		t.Fatal("the rule missed the text on the command line")
	}
	inVars := &run.Run{
		Tool: run.ToolAnsible, Playbook: "maintenance.yml",
		ExtraVars: map[string]any{"sql": "drop database prod"},
	}
	if rule.MatchesPlain(inVars) {
		t.Error("the command criterion reached into the extra variables, which is not what it is " +
			"documented to read")
	}
	// The risk grader does read them, so a rule that needs to catch this uses a risk floor.
	if got := run.AssessRisk(inVars).Level; got != run.RiskHigh {
		t.Errorf("AssessRisk() = %q, want high: the destructive text is in the variables", got)
	}
	if !strings.Contains(strings.Join(run.AssessRisk(inVars).Reasons, " "), "drop database") {
		t.Error("the risk reasons do not say what was found, so an approver is not told why")
	}
}
