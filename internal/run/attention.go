package run

import (
	"context"
	"time"
)

// QueueTimes is a run store that records when a waiting run last entered the queue, so a dashboard
// can measure how long the run has been waiting in its current state rather than since it was
// created. A run approved after a long hold, or put back after its worker was lost, has been
// waiting for a worker only since then.
type QueueTimes interface {
	// QueuedTimes returns, for each pending run that re-entered the queue after it was created, the
	// moment it did: released by an approval, released with its split, or put back by the lease
	// sweep after its worker stopped reporting. A pending run missing from the map has waited since
	// it was created.
	QueuedTimes(ctx context.Context) (map[string]time.Time, error)
}

// AwaitingStep describes a workflow approval step waiting for a decision, as a notification names
// it: which step, what the person deciding is asked, and what each answer runs next.
type AwaitingStep struct {
	// ID is the approval step's record, the id an approve or reject call takes.
	ID string `json:"id"`
	// Name is the step's name in the workflow.
	Name string `json:"name"`
	// Description is what the step's author asked the approver to decide.
	Description string `json:"description,omitempty"`
	// OnApprove names the steps an approval runs, in declaration order.
	OnApprove []string `json:"on_approve,omitempty"`
	// OnDeny names the steps a denial or a timeout runs, in declaration order.
	OnDeny []string `json:"on_deny,omitempty"`
	// ExpiresAt is when the step times out and takes its deny path, absent when it waits forever.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Clone returns a deep copy, nil for nil.
func (s *AwaitingStep) Clone() *AwaitingStep {
	if s == nil {
		return nil
	}
	out := *s
	out.OnApprove = append([]string(nil), s.OnApprove...)
	out.OnDeny = append([]string(nil), s.OnDeny...)
	if s.ExpiresAt != nil {
		at := *s.ExpiresAt
		out.ExpiresAt = &at
	}
	return &out
}

// AttentionNote describes why a run has needed attention longer than its alert threshold, as an
// alert names it. A schedule held back by the run carries its own id and name, since the alert is
// about the schedule's work as much as the run's.
type AttentionNote struct {
	// ID identifies the alert. The same condition on the same run keeps the same id for as long as
	// it lasts, so a receiver can collapse repeats.
	ID string `json:"id"`
	// Blocker is what is stopping the work: worker_lost, no_worker, approval_needed, or blocked.
	Blocker string `json:"blocker"`
	// Summary is the alert in one line, the text a chat message or a text message leads with.
	Summary string `json:"summary"`
	// Reason says what is stopping the work, in a sentence.
	Reason string `json:"reason"`
	// WhoCanAct says who can do something about it.
	WhoCanAct string `json:"who_can_act,omitempty"`
	// Next says what happens next if nobody acts.
	Next string `json:"next,omitempty"`
	// Since is when the work entered its current condition.
	Since time.Time `json:"since"`
	// WaitingSeconds is how long it has been in that condition when the alert was raised.
	WaitingSeconds int64 `json:"waiting_seconds"`
	// ThresholdSeconds is the alert threshold it passed.
	ThresholdSeconds int64 `json:"threshold_seconds"`
	// ScheduleID names the schedule held back by this run, empty when the alert is about the run.
	ScheduleID string `json:"schedule_id,omitempty"`
	// ScheduleName is that schedule's name.
	ScheduleName string `json:"schedule_name,omitempty"`
	// Path is the interface page that lists the condition, relative to the server's address.
	Path string `json:"path,omitempty"`
}

// Clone returns a copy, nil for nil.
func (n *AttentionNote) Clone() *AttentionNote {
	if n == nil {
		return nil
	}
	out := *n
	return &out
}

// The memory store records queue times like the database stores do, so a test against it measures
// waiting the way production does.
var _ QueueTimes = (*memStore)(nil)
