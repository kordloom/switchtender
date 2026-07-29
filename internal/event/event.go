// Package event models Ansible execution as a stream of typed events. The roundhouse callback
// plugin emits one JSON object per event and this package turns that stream into Go values that
// the API, storage, and UI render as a timeline and a per host status matrix.
package event

import "time"

// Type identifies what an event represents.
type Type string

const (
	// TypePlayStart marks the beginning of a play.
	TypePlayStart Type = "play_start"
	// TypeTaskStart marks the beginning of a task.
	TypeTaskStart Type = "task_start"
	// TypeRunnerOK marks a host completing a task successfully.
	TypeRunnerOK Type = "runner_ok"
	// TypeRunnerFailed marks a host failing a task.
	TypeRunnerFailed Type = "runner_failed"
	// TypeRunnerSkipped marks a host skipping a task.
	TypeRunnerSkipped Type = "runner_skipped"
	// TypeRunnerUnreachable marks a host that could not be reached.
	TypeRunnerUnreachable Type = "runner_unreachable"
	// TypeStats marks the end of run recap totals per host.
	TypeStats Type = "stats"
	// TypeFacts carries a host's gathered system facts from a fact-gathering task.
	TypeFacts Type = "facts"
)

// Event is one structured moment in a run.
type Event struct {
	// Type identifies what the event represents.
	Type Type `json:"type"`
	// Time is when the event occurred.
	Time time.Time `json:"time"`
	// Play is the play name, set on play, task, and runner events.
	Play string `json:"play,omitempty"`
	// Task is the task name, set on task and runner events.
	Task string `json:"task,omitempty"`
	// Host is the target host, set on runner events.
	Host string `json:"host,omitempty"`
	// Changed reports whether a runner event changed state on the host.
	Changed bool `json:"changed,omitempty"`
	// Message is the task result message, when present on a runner event.
	Message string `json:"message,omitempty"`
	// Stdout is captured standard output, when present on a runner event.
	Stdout string `json:"stdout,omitempty"`
	// Stderr is captured standard error, when present on a runner event.
	Stderr string `json:"stderr,omitempty"`
	// RC is the module return code, when present on a runner event.
	RC *int `json:"rc,omitempty"`
	// Diff is a captured change diff, when present on a runner event.
	Diff string `json:"diff,omitempty"`
	// Truncated reports that one or more captured fields were cut to a length cap.
	Truncated bool `json:"truncated,omitempty"`
	// Stats holds per host recap totals, set only on stats events.
	Stats map[string]HostStats `json:"stats,omitempty"`
	// Outputs holds values a playbook published with set_stats, set only on stats events. A
	// pipeline feeds them to dependent steps as extra vars.
	Outputs map[string]any `json:"outputs,omitempty"`
	// Facts holds a host's gathered system facts, set on a facts event.
	Facts map[string]string `json:"facts,omitempty"`
	// Seq is the store sequence assigned when the event is read, used as a stream and paging
	// cursor. It is zero on an event that has not come from a store, so it is never persisted.
	Seq int64 `json:"seq,omitempty"`
}

// HostStats holds the recap totals for a single host.
type HostStats struct {
	// OK is the count of successful tasks.
	OK int `json:"ok"`
	// Changed is the count of tasks that changed state.
	Changed int `json:"changed"`
	// Failures is the count of failed tasks.
	Failures int `json:"failures"`
	// Unreachable is the count of unreachable results.
	Unreachable int `json:"unreachable"`
	// Skipped is the count of skipped tasks.
	Skipped int `json:"skipped"`
	// Rescued is the count of rescued tasks.
	Rescued int `json:"rescued"`
	// Ignored is the count of ignored errors.
	Ignored int `json:"ignored"`
}
