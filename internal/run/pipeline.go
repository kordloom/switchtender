package run

import "slices"

// StepApproval is the step type of an approval step: a point in a workflow where it waits for a
// person to approve or deny before the steps after it run.
const StepApproval = "approval"

// PipelineStep describes one step of a pipeline run.
type PipelineStep struct {
	// Name identifies the step. Required when any step in the pipeline declares dependencies.
	Name string `json:"name"`
	// Type is empty for a step that runs a tool, or approval for a step that waits for a person.
	Type string `json:"type,omitempty"`
	// Description tells an approver what an approval step is asking them to decide. Unused by a step
	// that runs a tool.
	Description string `json:"description,omitempty"`
	// ApprovalTimeout is how many seconds an approval step waits for a decision before it times out
	// and takes its deny path. Zero waits until somebody decides.
	ApprovalTimeout int `json:"approval_timeout,omitempty"`
	// Playbook is the playbook the step runs under the Ansible tool.
	Playbook string `json:"playbook"`
	// Inventory overrides the pipeline inventory for this step when set.
	Inventory string `json:"inventory,omitempty"`
	// Tool selects the step's execution engine: ansible, bash, terraform, opentofu, python, powershell, or go. Empty means
	// ansible, so a pipeline can mix tools, for example a terraform step then an ansible step.
	Tool string `json:"tool,omitempty"`
	// Command carries the tool's primary input for non-Ansible steps: the script for bash and python,
	// the working directory for terraform.
	Command string `json:"command,omitempty"`
	// DryRun runs the step's tool in its no-change mode.
	DryRun bool `json:"dry_run,omitempty"`
	// ContinueOnFailure lets steps after or downstream of this one proceed even if it fails.
	ContinueOnFailure bool `json:"continue_on_failure,omitempty"`
	// Retries is how many extra attempts the step gets after a failure before it counts as
	// failed. Each attempt is its own run.
	Retries int `json:"retries,omitempty"`
	// DependsOn names the steps that must finish before this one starts. When no step in the
	// pipeline declares dependencies the steps run in order; when any step does, the pipeline is a
	// graph and steps without dependencies start immediately, in parallel. Naming an approval step
	// here means this step runs only once that approval is given.
	DependsOn []string `json:"depends_on,omitempty"`
	// IfDenied names approval steps whose denial this step handles: it runs only when every one of
	// them was denied or timed out, and is skipped when any was approved. It is the deny path of an
	// approval step, the way AWX runs a node's failure path.
	IfDenied []string `json:"if_denied,omitempty"`
}

// IsApproval reports whether the step waits for a person rather than running a tool.
func (s PipelineStep) IsApproval() bool {
	return s.Type == StepApproval
}

// HasApproval reports whether any step in the pipeline is an approval step.
func HasApproval(steps []PipelineStep) bool {
	for _, s := range steps {
		if s.IsApproval() {
			return true
		}
	}
	return false
}

// GraphSteps returns the steps as the dependency graph a pipeline holding an approval step is
// walked through. A pipeline that already declares dependencies is returned unchanged. A plain
// sequence gets each step depending on the one before it, which is exactly how a sequence runs: a
// step starts once the previous one succeeded or failed with continue on failure set, and a failure
// without it skips everything after. The steps are copied, so the stored graph is never edited.
func GraphSteps(steps []PipelineStep) []PipelineStep {
	out := make([]PipelineStep, len(steps))
	for i, s := range steps {
		s.DependsOn = append([]string(nil), s.DependsOn...)
		s.IfDenied = append([]string(nil), s.IfDenied...)
		out[i] = s
	}
	if pipelineHasDependencies(steps) {
		return out
	}
	for i := 1; i < len(out); i++ {
		out[i].DependsOn = []string{out[i-1].Name}
	}
	return out
}

// ClonePipelineSteps returns a copy of steps that shares no memory with them, so a change made to the
// copy never reaches the caller's slice.
func ClonePipelineSteps(steps []PipelineStep) []PipelineStep {
	if steps == nil {
		return nil
	}
	out := make([]PipelineStep, len(steps))
	for i, s := range steps {
		s.DependsOn = slices.Clone(s.DependsOn)
		s.IfDenied = slices.Clone(s.IfDenied)
		out[i] = s
	}
	return out
}
