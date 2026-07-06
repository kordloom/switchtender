// Package run holds the Yardmaster run domain model and its persistence interface.
// A run records one execution of an engine (playbook) against a manifest (inventory).
package run

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"time"
)

// Status is the lifecycle state of a run.
type Status string

const (
	// StatusPending means the run is accepted but not yet started.
	StatusPending Status = "pending"
	// StatusRunning means the run is executing.
	StatusRunning Status = "running"
	// StatusSucceeded means the run finished with a zero exit code.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the run finished with a non-zero exit code or could not start.
	StatusFailed Status = "failed"
	// StatusCanceled means the run was stopped before completion.
	StatusCanceled Status = "canceled"
	// StatusInterrupted means the run was abandoned when the server stopped and cannot resume.
	StatusInterrupted Status = "interrupted"
)

const (
	// KindSplit marks a parent run whose children are inventory shards.
	KindSplit = "split"
	// KindPipeline marks a parent run whose children are pipeline steps.
	KindPipeline = "pipeline"
)

// Terminal reports whether the status is a final state.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusInterrupted:
		return true
	default:
		return false
	}
}

// Run is a single execution of a playbook against an inventory.
type Run struct {
	// ID is the unique run identifier.
	ID string `json:"id"`
	// Playbook is the path to the Ansible playbook to execute.
	Playbook string `json:"playbook"`
	// Inventory is the path to the Ansible inventory to target.
	Inventory string `json:"inventory"`
	// Status is the current lifecycle state.
	Status Status `json:"status"`
	// ExitCode is the process exit code, set once the run reaches a terminal state.
	ExitCode *int `json:"exit_code,omitempty"`
	// Error holds a failure detail when the run could not start.
	Error string `json:"error,omitempty"`
	// CreatedAt is when the run was accepted.
	CreatedAt time.Time `json:"created_at"`
	// StartedAt is when execution began.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// EndedAt is when execution finished.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// ParentID links a shard run to its parent split run.
	ParentID *string `json:"parent_id,omitempty"`
	// ShardIndex is this shard's position within the split, zero based.
	ShardIndex *int `json:"shard_index,omitempty"`
	// ShardCount is the total number of shards in the split.
	ShardCount *int `json:"shard_count,omitempty"`
	// Limit restricts execution to a host pattern, used to target a shard's hosts.
	Limit string `json:"limit,omitempty"`
	// Kind distinguishes a plain run from a split or pipeline parent. Empty means a plain run.
	Kind string `json:"kind,omitempty"`
	// RetryOf links a split created by a failed shard retry back to the run it retries.
	RetryOf *string `json:"retry_of,omitempty"`
	// StepName is the step name when this run is a pipeline step.
	StepName string `json:"step_name,omitempty"`
	// StepIndex is the step order when this run is a pipeline step.
	StepIndex *int `json:"step_index,omitempty"`
	// Attempt is the retry attempt number for a pipeline step run, zero for the first try.
	Attempt int `json:"attempt,omitempty"`
	// ExtraVars are the variables injected into the run, for a pipeline step the merged outputs
	// of the steps it depends on.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Outputs are the values the playbook published with set_stats for downstream steps.
	Outputs map[string]any `json:"outputs,omitempty"`
	// ClaimedBy names the process that leased this run for execution, empty while queued.
	ClaimedBy string `json:"claimed_by,omitempty"`
	// ClaimedAt is when the lease was taken or last renewed.
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	// CancelRequested asks whichever process holds the run to stop it.
	CancelRequested bool `json:"cancel_requested,omitempty"`
	// CredentialIDs names the stored credentials materialized for this run.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ProjectID names the git project the playbook and inventory paths resolve inside.
	ProjectID string `json:"project_id,omitempty"`
	// CommitSHA is the exact commit the run executed, stamped after the project sync.
	CommitSHA string `json:"commit_sha,omitempty"`
}

// Clone returns a deep copy so callers cannot mutate stored state through shared pointers.
func (r *Run) Clone() *Run {
	if r == nil {
		return nil
	}
	out := *r
	if r.ExitCode != nil {
		code := *r.ExitCode
		out.ExitCode = &code
	}
	if r.StartedAt != nil {
		t := *r.StartedAt
		out.StartedAt = &t
	}
	if r.EndedAt != nil {
		t := *r.EndedAt
		out.EndedAt = &t
	}
	if r.ParentID != nil {
		id := *r.ParentID
		out.ParentID = &id
	}
	if r.ShardIndex != nil {
		i := *r.ShardIndex
		out.ShardIndex = &i
	}
	if r.ShardCount != nil {
		c := *r.ShardCount
		out.ShardCount = &c
	}
	if r.StepIndex != nil {
		i := *r.StepIndex
		out.StepIndex = &i
	}
	if r.RetryOf != nil {
		id := *r.RetryOf
		out.RetryOf = &id
	}
	out.ExtraVars = maps.Clone(r.ExtraVars)
	out.Outputs = maps.Clone(r.Outputs)
	if r.ClaimedAt != nil {
		t := *r.ClaimedAt
		out.ClaimedAt = &t
	}
	out.CredentialIDs = append([]string(nil), r.CredentialIDs...)
	return &out
}

// SubmitOption customizes a run at submission.
type SubmitOption func(*Run)

// WithCredentialIDs attaches stored credentials to the run.
func WithCredentialIDs(ids []string) SubmitOption {
	return func(r *Run) { r.CredentialIDs = append([]string(nil), ids...) }
}

// WithProject sources the run's playbook and inventory paths from a git project.
func WithProject(id string) SubmitOption {
	return func(r *Run) { r.ProjectID = id }
}

// WithExtraVars injects variables into the run.
func WithExtraVars(vars map[string]any) SubmitOption {
	return func(r *Run) {
		if len(vars) == 0 {
			return
		}
		r.ExtraVars = maps.Clone(vars)
	}
}

// ApplyOptions applies opts to r.
func ApplyOptions(r *Run, opts []SubmitOption) {
	for _, opt := range opts {
		opt(r)
	}
}

// NewID returns a random run identifier prefixed with "run_".
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("run: read random: " + err.Error())
	}
	return "run_" + hex.EncodeToString(b[:])
}
