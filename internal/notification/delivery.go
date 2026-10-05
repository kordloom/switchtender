package notification

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/util"
)

// Delivery statuses.
const (
	// DeliveryPending is a delivery not yet made: waiting for its turn, for its next attempt, or
	// being attempted now.
	DeliveryPending = "pending"
	// DeliveryDelivered is a delivery the target accepted.
	DeliveryDelivered = "delivered"
	// DeliveryFailed is a delivery whose attempts ran out, or that could never succeed, such as one
	// to a target whose channel the server has no transport for.
	DeliveryFailed = "failed"
	// DeliverySkipped is a delivery never attempted, because the target was waiting for its secret
	// when the event happened, or was changed before the delivery to a kind the event does not
	// reach.
	DeliverySkipped = "skipped"
)

// MaxAttempts is how many times a delivery is attempted before it is marked failed and the
// deliveries after it to the same target for the same run go ahead.
const MaxAttempts = 5

// retryDelays are the waits before the second, third, fourth, and fifth attempts: about three
// minutes in all, which rides out a restart of the receiving end without holding the notifications
// after it for longer than an operator would wait to hear about a run.
var retryDelays = []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 2 * time.Minute}

// RetryDelay returns how long to wait after the given number of failed attempts before the next
// one.
func RetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		return 0
	}
	if attempts > len(retryDelays) {
		return retryDelays[len(retryDelays)-1]
	}
	return retryDelays[attempts-1]
}

// Millis returns t at the millisecond resolution the stores keep delivery times in, in UTC, so
// every store hands back the same instant it was given.
func Millis(t time.Time) time.Time {
	return time.UnixMilli(t.UnixMilli()).UTC()
}

// EarlierFailedNote is the sentence a delivery carries when an earlier notification about the same
// run to the same target failed, so a receiver reading a later message knows one is missing.
const EarlierFailedNote = "An earlier notification about this run to this target could not be " +
	"delivered."

// RunEvent is one moment in a run that notification targets are told about. Seq numbers a run's
// events in the order the run reached them, starting at one, and is what delivery is ordered by:
// a timestamp from more than one process cannot be.
type RunEvent struct {
	// RunID is the run the event belongs to.
	RunID string `json:"run_id"`
	// Seq is the event's position in the run's own sequence, assigned when it is recorded.
	Seq int64 `json:"seq"`
	// Event is started, success, failure, or approval.
	Event string `json:"event"`
	// Branch names the workflow step the event came from, empty for the run's own lifecycle. Events
	// on branches of one workflow that run beside each other are not put in an order between
	// themselves; each still follows every earlier event of the run's own lifecycle and of the
	// steps in Follows.
	Branch string `json:"branch,omitempty"`
	// Follows names the steps Branch comes after in the workflow, its transitive dependencies, so
	// their events reach a target before this one does.
	Follows []string `json:"follows,omitempty"`
	// Snapshot is the run as it stood at the event, redacted the way everything sent off the host
	// is, so a message delivered later still says what was true then.
	Snapshot []byte `json:"-"`
	// CreatedAt is when the event was recorded.
	CreatedAt time.Time `json:"created_at"`
}

// DedupeKey returns the key an event is recorded under at most once per run: its event, its branch,
// and a digest of its snapshot. The same moment announced twice, by a retried report or by two
// processes that both saw it, carries the same snapshot and is recorded once, while two different
// moments of the same kind, such as two approval steps of one workflow, differ in what they say and
// are both kept. No list of event names is consulted, so an event added later is ordered and
// deduplicated the same way as the ones that exist now.
func (ev *RunEvent) DedupeKey() string {
	sum := sha256.Sum256(ev.Snapshot)
	return ev.Event + "/" + ev.Branch + "/" + hex.EncodeToString(sum[:8])
}

// End reports whether the event is the end of the run's own lifecycle: a success or a failure on no
// branch. A run ends once, so a store records one end per run however often and from whatever copy
// of the run it is announced: by the process that finished the run, and again by the sweep that
// finds the end owed when that process stopped before it was recorded.
func (ev *RunEvent) End() bool {
	return ev.Branch == "" && IsEnd(ev.Event)
}

// Once returns the events that count as the same moment as ev when the run already holds one on
// ev's branch, so a store records ev only once: success and failure for the end of the run's own
// lifecycle, started for its start, and approval for a hold, of the run itself or of one workflow
// step. A run starts once, is held once in its own lifecycle, and waits at each approval step once,
// so each of those reaches each target once however many processes announce it, from whatever copy
// of the run: the process that moved the run, and the sweep that found the move owed when that
// process stopped before recording it. Any other event, such as an attention alert, which can be
// raised again, returns two empty names, and is kept once per DedupeKey alone.
func (ev *RunEvent) Once() (string, string) {
	switch {
	case ev.End():
		return EventSuccess, EventFailure
	case ev.Event == EventStarted && ev.Branch == "":
		return EventStarted, EventStarted
	case ev.Event == EventApproval:
		return EventApproval, EventApproval
	}
	return "", ""
}

// IsEnd reports whether an event names the end of a run, a success or a failure.
func IsEnd(event string) bool {
	return event == EventSuccess || event == EventFailure
}

// Ordered reports whether an earlier event on the branch earlier has to reach a target before an
// event on branch, which comes after the steps in follows. An event of the run's own lifecycle, on
// no branch, follows every earlier event and is followed by every later one. An event follows the
// earlier events of its own step and of the steps it comes after. Two events on steps that run
// beside each other happened concurrently, and neither waits for the other, since putting them in
// an order would be inventing one.
func Ordered(branch string, follows []string, earlier string) bool {
	return branch == "" || earlier == "" || branch == earlier || slices.Contains(follows, earlier)
}

// followsSep separates the step names a stored delivery follows. A step name with a line break in
// it would only make delivery wait for more than it needs to, never for less.
const followsSep = "\n"

// EncodeFollows renders the steps an event follows the way a store keeps them: each name between
// line breaks, so a store can ask whether a step is among them with one exact substring search.
func EncodeFollows(follows []string) string {
	if len(follows) == 0 {
		return ""
	}
	return followsSep + strings.Join(follows, followsSep) + followsSep
}

// DecodeFollows reads what EncodeFollows rendered.
func DecodeFollows(encoded string) []string {
	trimmed := strings.Trim(encoded, followsSep)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, followsSep)
}

// Branch places an event within a workflow: the step it came from, and the steps that step comes
// after. The zero Branch is the run's own lifecycle.
type Branch struct {
	// Step names the workflow step, empty for the run's own lifecycle.
	Step string
	// Follows names the steps Step comes after in the workflow, its transitive dependencies.
	Follows []string
}

// Recipient is one target an event is recorded for.
type Recipient struct {
	// NotificationID is the target.
	NotificationID string
	// Name is the target's name when the event was recorded, kept so the record still reads after
	// the target is renamed or deleted.
	Name string
	// Kind is the target's channel kind.
	Kind string
	// Skip says why the delivery is recorded as skipped rather than queued, empty to queue it.
	Skip string
}

// Delivery is one event of a run on its way to one target. It is keyed by the target, the run, and
// the event's sequence number, so however many workers deliver and however often one retries, an
// event reaches a target at most once per attempt and is never recorded twice.
type Delivery struct {
	// NotificationID is the target.
	NotificationID string `json:"notification_id"`
	// RunID is the run the event belongs to.
	RunID string `json:"run_id"`
	// Seq is the event's position in the run's sequence.
	Seq int64 `json:"seq"`
	// Event is started, success, failure, or approval.
	Event string `json:"event"`
	// Branch names the workflow step the event came from, empty for the run's own lifecycle.
	Branch string `json:"branch,omitempty"`
	// Follows names the steps Branch comes after, whose deliveries to the same target go first.
	Follows []string `json:"-"`
	// TargetName is the target's name when the event was recorded.
	TargetName string `json:"target_name"`
	// TargetKind is the target's channel kind when the event was recorded.
	TargetKind string `json:"target_kind"`
	// Status is pending, delivered, failed, or skipped.
	Status string `json:"status"`
	// Attempts is how many attempts have been made.
	Attempts int `json:"attempts"`
	// NextAttemptAt is when a pending delivery may next be attempted.
	NextAttemptAt time.Time `json:"next_attempt_at"`
	// LastError is why the latest attempt failed, or why the delivery was skipped. It never holds
	// the target's address or key.
	LastError string `json:"last_error,omitempty"`
	// Note is the sentence the delivered message carried beyond the event itself, such as
	// EarlierFailedNote.
	Note string `json:"note,omitempty"`
	// CreatedAt is when the delivery was recorded.
	CreatedAt time.Time `json:"created_at"`
	// FinishedAt is when it was delivered, failed for good, or skipped.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// ClaimedBy names the worker holding the delivery for an attempt, empty when none does.
	ClaimedBy string `json:"-"`
	// ClaimUntil is when the worker's claim lapses.
	ClaimUntil *time.Time `json:"-"`
	// Snapshot is the run as it stood at the event, filled in by Claim for the worker delivering
	// it.
	Snapshot []byte `json:"-"`
	// EarlierFailed reports, on a claimed delivery, that an earlier delivery to the same target for
	// the same run failed, so this one carries EarlierFailedNote.
	EarlierFailed bool `json:"-"`
}

// Key returns the delivery's idempotency key: its target, its run, and its event's sequence number.
func (d *Delivery) Key() string {
	return d.NotificationID + "/" + d.RunID + "/" + strconv.FormatInt(d.Seq, 10)
}

// Clone returns a copy, so a caller cannot change stored state through what a store handed it.
func (d *Delivery) Clone() *Delivery {
	if d == nil {
		return nil
	}
	out := *d
	if d.FinishedAt != nil {
		t := *d.FinishedAt
		out.FinishedAt = &t
	}
	if d.ClaimUntil != nil {
		t := *d.ClaimUntil
		out.ClaimUntil = &t
	}
	out.Snapshot = append([]byte(nil), d.Snapshot...)
	out.Follows = slices.Clone(d.Follows)
	return &out
}

// Outcome is what one attempt at a claimed delivery came to.
type Outcome struct {
	// Status is DeliveryDelivered, DeliveryFailed, DeliverySkipped for a delivery its target no
	// longer hears, or DeliveryPending for an attempt that failed and will be retried.
	Status string
	// Error is why the attempt failed or the delivery was skipped, empty when it was delivered.
	Error string
	// Note is the sentence the attempt's message carried.
	Note string
	// At is when the attempt ended.
	At time.Time
	// NextAttemptAt is when a retried delivery may next be attempted.
	NextAttemptAt time.Time
}

// SanitizeText replaces anything in the attempt's text that a text column cannot hold. The error is
// what a mail server or an endpoint answered, which is somebody else's bytes: an SMTP reply holding
// a NUL failed this write on PostgreSQL, the delivery stayed claimed until its claim ran out, and
// it was retried and failed the same way for as long as the server ran.
func (o *Outcome) SanitizeText() {
	o.Error = util.SafeText(o.Error)
	o.Note = util.SafeText(o.Note)
}

// DeliveryFilter selects deliveries to list.
type DeliveryFilter struct {
	// RunID keeps one run's deliveries when set.
	RunID string
	// NotificationID keeps one target's deliveries when set.
	NotificationID string
	// Status keeps deliveries in one status when set.
	Status string
	// Since keeps deliveries recorded at or after it when set.
	Since time.Time
	// Limit bounds how many are returned. Zero or less means DefaultDeliveryLimit.
	Limit int
}

// DefaultDeliveryLimit bounds a delivery listing that names no limit.
const DefaultDeliveryLimit = 200

// EffectiveLimit returns how many deliveries the filter lets a listing return.
func (f DeliveryFilter) EffectiveLimit() int {
	if f.Limit <= 0 || f.Limit > DefaultDeliveryLimit {
		return DefaultDeliveryLimit
	}
	return f.Limit
}

// Permanent marks a delivery error that no retry can fix, such as a channel the server has no
// transport for or a target the receiving end rejected as unknown, so the delivery is marked failed
// at once rather than attempted again.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err was marked by Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// permanentError wraps an error no retry can fix.
type permanentError struct {
	// err is the underlying failure.
	err error
}

// Error returns the underlying failure's message.
func (p *permanentError) Error() string { return p.err.Error() }

// Unwrap returns the underlying failure.
func (p *permanentError) Unwrap() error { return p.err }
