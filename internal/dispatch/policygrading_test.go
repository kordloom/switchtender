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

// gradedAssign matches the declaration of a local that holds a graded run, and captures what it is
// assigned.
var gradedAssign = regexp.MustCompile(`\b(gr|gp|gs)\s*:=\s*(.+)$`)

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
				if gradedExpr(name, arg) || isGradedLocal(arg) {
					continue
				}
				ungraded = append(ungraded, name+": "+strings.TrimSpace(line))
			}
			// A local is only as good as what it was assigned, so its declaration is held to the
			// same rule. Accepting the name alone let a local holding a run graded without the
			// project checkout pass for a graded one.
			if m := gradedAssign.FindStringSubmatch(line); m != nil && !gradedExpr(name, m[2]) {
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

// gradedExpr reports whether expr grades a run the way the file it sits in must. A dispatcher
// method grades through d.graded, which reads a project run's playbook from its checkout. Only the
// plan gate's free functions may grade without a checkout, since their runs are Terraform or
// OpenTofu applies that name no playbook: anywhere else, grading without the checkout is the
// ungraded original under another name for every run drawn from a project.
func gradedExpr(file, expr string) bool {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "d.graded(") {
		return true
	}
	return file == "plangate.go" && strings.HasPrefix(expr, "gradedLocally(")
}
