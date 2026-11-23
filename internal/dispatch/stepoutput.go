package dispatch

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kordloom/switchtender/internal/run"
)

// layerOutputs copies a step's published outputs over vars and records in from that step as the
// origin of each key, so a later step can tell a value the run was approved with from one a step
// published while it ran.
func layerOutputs(vars map[string]any, from map[string]string, step string, outputs map[string]any) {
	maps.Copy(vars, outputs)
	for k := range outputs {
		from[k] = step
	}
}

// outputRefusal reports why step may not start with the published values in vars, or an empty
// string when it may. from names the step that published each key still in force.
//
// A published value reaches an Ansible step as an extra var, and Ansible renders an extra var on
// the controller. A step's outputs are whatever its play printed with set_stats, which can echo a
// host's facts or a file it read, so a value holding template code ran a lookup on the controller
// in the next step, and a key named ansible_ chose the next step's interpreter or connection. The
// workflow was approved with its own vars, never with these. A step that is not Ansible reads its
// vars as plain values, so it is not refused. Keys are checked in order, so the reason is stable.
func outputRefusal(step run.PipelineStep, vars map[string]any, from map[string]string) string {
	if step.IsApproval() || run.NormalizeTool(step.Tool) != run.ToolAnsible {
		return ""
	}
	for _, k := range slices.Sorted(maps.Keys(from)) {
		switch {
		case strings.HasPrefix(strings.ToLower(k), "ansible_"):
			return fmt.Sprintf("step %q cannot take %q from step %q: a published name that begins "+
				"with ansible_ would set how Ansible connects or runs, so the workflow stopped",
				step.Name, k, from[k])
		case run.HoldsTemplate(map[string]any{k: vars[k]}):
			return fmt.Sprintf("step %q cannot take %q from step %q: the value holds template "+
				"code, which Ansible would render on the controller, so the workflow stopped",
				step.Name, k, from[k])
		}
	}
	return ""
}
