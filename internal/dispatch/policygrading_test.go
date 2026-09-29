package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// policyCall matches a policy evaluation and captures the run expression it is given.
var policyCall = regexp.MustCompile(`policy\.(Denying|Requiring|RequireDistinct)\(policies, ([^)]*)`)

// TestEveryPolicyEvaluationSeesTheGradedRun is a guard against the way this feature was nearly
// shipped broken, twice.
//
// A policy can hold a run on whether it can be taken back. That grade is blind to an Ansible
// playbook unless the caller reads the file first, so every evaluation has to pass the graded copy.
// Wiring them up by hand missed four of them on the first pass, when a script failed partway and
// the remaining edits were silently dropped, and then missed a whole file and one line inside a
// block whose neighbor was already correct.
//
// Each miss is invisible: the rule still loads, still lists, and simply never fires on the runs it
// was written for. Nothing in a build or a passing test would have said so, which is exactly why
// this reads the source rather than trusting that the next person remembers.
func TestEveryPolicyEvaluationSeesTheGradedRun(t *testing.T) {
	t.Parallel()
	dir, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	var ungraded []string
	var seen int
	for _, entry := range dir {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(".", name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		for _, line := range strings.Split(string(body), "\n") {
			for _, m := range policyCall.FindAllStringSubmatch(line, -1) {
				seen++
				arg := strings.TrimSpace(m[2])
				// Either the graded copy inline, or a local holding one. A bare run expression is
				// the bug: it is the ungraded original.
				if strings.HasPrefix(arg, "graded(") || isGradedLocal(arg) {
					continue
				}
				ungraded = append(ungraded, name+": "+strings.TrimSpace(line))
			}
		}
	}
	if seen == 0 {
		t.Fatal("no policy evaluations found, so this guard is asserting nothing. The call shape " +
			"changed and this pattern has to change with it")
	}
	if len(ungraded) > 0 {
		t.Errorf("%d policy evaluation(s) use the ungraded run, so a reversibility rule will not "+
			"fire on an Ansible playbook there:\n  %s",
			len(ungraded), strings.Join(ungraded, "\n  "))
	}
}

// gradedLocals are the local variable names this package uses to hold a graded run. Naming them is
// deliberate: a new name has to be added here, which is a moment to check it really is graded.
var gradedLocals = map[string]bool{"gr": true, "gp": true, "gs": true, "gu": true}

// isGradedLocal reports whether an argument names a local already holding a graded run.
func isGradedLocal(arg string) bool { return gradedLocals[arg] }
