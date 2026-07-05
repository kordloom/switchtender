// Package run holds the Yardmaster run domain model and its persistence interface.
// A run records one execution of an engine (playbook) against a manifest (inventory).
package run

import (
	"crypto/rand"
	"encoding/hex"
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
	return &out
}

// NewID returns a random run identifier prefixed with "run_".
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("run: read random: " + err.Error())
	}
	return "run_" + hex.EncodeToString(b[:])
}
