package run

// PipelineStep describes one step of a pipeline run.
type PipelineStep struct {
	// Name identifies the step. Required when any step in the pipeline declares dependencies.
	Name string `json:"name"`
	// Playbook is the playbook the step runs.
	Playbook string `json:"playbook"`
	// Inventory overrides the pipeline inventory for this step when set.
	Inventory string `json:"inventory,omitempty"`
	// ContinueOnFailure lets steps after or downstream of this one proceed even if it fails.
	ContinueOnFailure bool `json:"continue_on_failure,omitempty"`
	// Retries is how many extra attempts the step gets after a failure before it counts as
	// failed. Each attempt is its own run.
	Retries int `json:"retries,omitempty"`
	// DependsOn names the steps that must finish before this one starts. When no step in the
	// pipeline declares dependencies the steps run in order; when any step does, the pipeline is a
	// graph and steps without dependencies start immediately, in parallel.
	DependsOn []string `json:"depends_on,omitempty"`
}
