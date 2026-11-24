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
// string when it may. parent is the workflow run, whose extra vars and secret survey answers are
// what a person approved. from names the step that published each key still in force.
//
// A published key that names a var the workflow was approved with is refused for every step that
// is not an approval, whatever its tool. A step's outputs are whatever its play printed with
// set_stats, which can echo a host's facts or a file it read, so a host could swap an approved
// target_env or confirm flag for a plain value of its own, and the step would run with a value no
// person saw. A value equal to the approved one is refused too, so the rule has one meaning. An
// output replacing an earlier step's output of the same name stays legal.
//
// A published value reaches an Ansible step as an extra var, and Ansible renders an extra var on
// the controller, so a value holding template code ran a lookup on the controller in the next
// step, and a key named ansible_ chose the next step's interpreter or connection. A step that is
// not Ansible reads its vars as plain values, so those two checks apply to Ansible steps only.
// Keys are checked in order, so the reason is stable.
func outputRefusal(step run.PipelineStep, parent *run.Run, vars map[string]any, from map[string]string) string {
	if step.IsApproval() {
		return ""
	}
	keys := slices.Sorted(maps.Keys(from))
	for _, k := range keys {
		if approvedVar(parent, k) {
			return fmt.Sprintf("step %q cannot take %q from step %q: the workflow was approved with "+
				"its own value for that name, and a published value cannot replace it, so the "+
				"workflow stopped. Publish it under another name, or remove %q from the "+
				"workflow's vars", step.Name, k, from[k], k)
		}
	}
	if run.NormalizeTool(step.Tool) != run.ToolAnsible {
		return ""
	}
	for _, k := range keys {
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

// approvedVar reports whether name is one of the workflow's own extra vars or secret survey
// answers, the values a person approved it with.
func approvedVar(parent *run.Run, name string) bool {
	if parent == nil {
		return false
	}
	if _, ok := parent.ExtraVars[name]; ok {
		return true
	}
	return slices.Contains(parent.SealedNames, name)
}
