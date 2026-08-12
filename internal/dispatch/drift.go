package dispatch

import (
	"context"
	"regexp"
	"strconv"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

var (
	// planSummary matches a Terraform or OpenTofu plan's change summary line whole, capturing its
	// list of count clauses. The clause list is not fixed: Terraform 1.5 prints a leading "N to
	// import" clause ahead of add, change, and destroy, and later versions add more, so a pattern
	// pinned to three clauses in a fixed order fails to match any plan that imports anything.
	//
	// Like planNoChanges it is anchored to column zero, because the plan prints its own summary
	// flush left while anything quoted inside a resource diff is indented. A multi-line string
	// attribute renders as an indented heredoc, so accepting leading whitespace let a line of text
	// belonging to a resource stand in for the plan's verdict.
	planSummary = regexp.MustCompile(
		`(?m)^Plan:[ \t]+((?:\d+ to [a-z]+,[ \t]+)*\d+ to [a-z]+)\.?[ \t\r]*$`)

	// planClause matches one "N to verb" count clause within a plan's change summary line.
	planClause = regexp.MustCompile(`(\d+) to ([a-z]+)`)

	// planNoChanges matches what Terraform and OpenTofu print in place of a summary when a plan
	// would change nothing. It is anchored to the start of a line with no leading whitespace,
	// because the sentence is printed at column zero while anything quoted inside a plan diff is
	// indented. Accepting an indented match let a plan whose real summary was unreadable report a
	// confident zero it had not earned, which is the one way this parser could still fail open.
	planNoChanges = regexp.MustCompile(`(?m)^No changes\.`)

	// planOutputsOnly matches the heading Terraform prints when a plan changes only output values.
	// Such a plan has no summary line at all and destroys nothing, so it is a real zero rather than
	// an unreadable one. Without this it was held for approval against a limit it could not exceed,
	// and the reason recorded for the approver said the summary was unreadable, which was untrue.
	planOutputsOnly = regexp.MustCompile(`(?m)^Changes to Outputs:`)
)

// planCounts holds the resource counts a plan's change summary reported.
type planCounts struct {
	// Total is the sum of every count clause on the summary line.
	Total int
	// Destroy is the number of resources the plan would destroy.
	Destroy int
}

// recordPlanDrift records a drift signal for a dry run whose plan found pending changes, keyed on the
// working directory so it lands on the Drift page beside Ansible hosts. The changed-resource count is
// read from the run's captured plan output, falling back to one so the row still shows as drifted. It
// is best effort: a failure is logged and does not affect the run result.
func (d *Dispatcher) recordPlanDrift(r *run.Run) {
	changes := d.planChanges(r.ID)
	if changes < 1 {
		changes = 1
	}
	host := r.Command
	if host == "" {
		host = r.ID
	}
	summary := run.HostSummary{Host: host, Changed: changes, Worst: "changed", RanAt: r.CreatedAt}
	if err := withRetries(func() error {
		return d.store.SaveHostSummary(context.Background(), r.ID, []run.HostSummary{summary})
	}); err != nil {
		d.log.Error("dispatch: save plan drift: "+err.Error(), zap.String("run_id", r.ID))
	}
}

// planChanges reads the run's captured output and returns the total resource changes its plan
// reported, or zero when the summary line is absent or the log cannot be read.
func (d *Dispatcher) planChanges(runID string) int {
	out, err := d.store.Log(context.Background(), runID)
	if err != nil {
		return 0
	}
	return parsePlanChanges(string(out))
}

// parsePlanChanges sums every count clause on a plan's summary line, returning zero when no summary
// is found and zero for a plan that reports no changes.
func parsePlanChanges(out string) int {
	counts, _ := parsePlanSummary(out)
	return counts.Total
}

// parsePlanSummary reads the change summary out of a plan's captured output. The second result
// reports whether a summary was read at all. False means the plan's effect is unknown, which is not
// the same as a plan that changes nothing, so a caller weighing destruction must not treat it as one.
//
// Every column-zero summary in the output is read, and the last one wins. Column zero alone is not
// enough, because output can legitimately carry more than one such line, so summaries that disagree
// are reported unreadable rather than resolved by guessing which one the plan meant.
func parsePlanSummary(out string) (planCounts, bool) {
	matches := planSummary.FindAllStringSubmatch(out, -1)
	if matches == nil {
		return planCounts{}, planNoChanges.MatchString(out) || planOutputsOnly.MatchString(out)
	}
	last := parsePlanClauses(matches[len(matches)-1][1])
	for _, m := range matches[:len(matches)-1] {
		if parsePlanClauses(m[1]) != last {
			return planCounts{}, false
		}
	}
	return last, true
}

// parsePlanClauses totals the "N to verb" count clauses of one plan summary line, keeping the
// destroy clause apart so a caller can weigh destruction rather than total change.
func parsePlanClauses(line string) planCounts {
	var counts planCounts
	for _, clause := range planClause.FindAllStringSubmatch(line, -1) {
		n, err := strconv.Atoi(clause[1])
		if err != nil {
			continue
		}
		counts.Total += n
		if clause[2] == "destroy" {
			counts.Destroy = n
		}
	}
	return counts
}
