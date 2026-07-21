package dispatch

import (
	"context"
	"regexp"
	"strconv"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
)

// planSummary matches a Terraform or OpenTofu plan's change summary line, so a drift check can report
// how many resources would change.
var planSummary = regexp.MustCompile(`Plan:\s+(\d+) to add,\s+(\d+) to change,\s+(\d+) to destroy`)

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

// parsePlanChanges sums the add, change, and destroy counts from a plan's summary line, returning
// zero when no summary is found.
func parsePlanChanges(out string) int {
	m := planSummary.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	total := 0
	for _, s := range m[1:] {
		n, _ := strconv.Atoi(s)
		total += n
	}
	return total
}
