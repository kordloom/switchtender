package run

// PipelineStep describes one step of a pipeline run.
type PipelineStep struct {
	// Name identifies the step.
	Name string `json:"name"`
	// Playbook is the playbook the step runs.
	Playbook string `json:"playbook"`
	// Inventory overrides the pipeline inventory for this step when set.
	Inventory string `json:"inventory,omitempty"`
	// ContinueOnFailure lets the pipeline proceed to the next step even if this step fails.
	ContinueOnFailure bool `json:"continue_on_failure,omitempty"`
}
