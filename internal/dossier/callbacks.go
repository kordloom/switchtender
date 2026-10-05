package dossier

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/audit"
)

// factCacheText states a run's fact cache setting the way the approval view states it, so the
// evidence an auditor reads and the sentence the approver read cannot describe it differently.
func factCacheText(timeoutSeconds int) string {
	if timeoutSeconds > 0 {
		return fmt.Sprintf("uses cached facts (timeout %ds): facts gathered more than %d seconds "+
			"earlier were gathered again", timeoutSeconds, timeoutSeconds)
	}
	return "uses cached facts (no timeout): cached facts were served however old they were"
}

// awxArrivalMarker is what the chain path of a provisioning callback that arrived on the
// AWX-compatible address carries before the AWX job template id it called.
const awxArrivalMarker = "/callback/fired/awx/"

// withAWXArrival adds, beside the run's source, the AWX-compatible address a provisioning callback
// arrived on, read from the chain entry that recorded the launch. A callback to the template's own
// address, and every other run, adds nothing.
func withAWXArrival(rows []metaRow, launch *audit.Entry) []metaRow {
	if launch == nil {
		return rows
	}
	_, tail, ok := strings.Cut(launch.Path, awxArrivalMarker)
	if !ok {
		return rows
	}
	id, err := strconv.ParseInt(tail, 10, 64)
	if err != nil || id <= 0 {
		return rows
	}
	row := metaRow{K: "Called back through", V: "the AWX-compatible address " +
		"/api/v2/job_templates/" + strconv.FormatInt(id, 10) + "/callback/"}
	for i, r := range rows {
		if r.K == "Source" {
			return append(rows[:i+1], append([]metaRow{row}, rows[i+1:]...)...)
		}
	}
	return append(rows, row)
}
