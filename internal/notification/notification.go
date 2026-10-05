// Package notification holds named notification targets, defined once and attached to the objects
// whose runs they should hear about, the way AWX attaches a notification template.
//
// A target is one channel of a kind the dispatcher already delivers to: a webhook, a chat webhook,
// ntfy, PagerDuty, Grafana, Twilio, or email. Its address and key are sealed with the install's
// encryption key before they are stored and are never returned by the API, only a masked hint of
// the address. An attachment ties a target to a template, a workflow, a schedule, a project, or an
// organization for one event: a run starting, succeeding, failing, being held for approval, or
// needing attention past its alert threshold, or a schedule's fire being skipped because its
// inventory matched no hosts. When a run reaches that event, every target attached to anything the
// run came from hears about it once.
//
// Targets sit beside the two older ways of routing a run, the server-wide channels set by flags and
// the per-template list a template carries inline, and replace neither.
package notification

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// Events a target can be attached for.
const (
	// EventStarted is a top-level run beginning to execute.
	EventStarted = "started"
	// EventSuccess is a top-level run finishing successfully.
	EventSuccess = "success"
	// EventFailure is a top-level run ending any other way: failed, interrupted, canceled, or
	// rejected.
	EventFailure = "failure"
	// EventApproval is a top-level run held for a person to approve, or a workflow waiting at one of
	// its approval steps.
	EventApproval = "approval"
	// EventSkipped is a schedule's fire skipped because its inventory matched no hosts, so no run
	// was started. A target attached to the schedule for failure hears it too.
	EventSkipped = "skipped"
	// EventAttention is a top-level run that has needed attention longer than its alert threshold:
	// its worker was lost and not reclaimed, no worker serves its queue, it is blocked, or an
	// approval has waited past an age alert someone turned on.
	EventAttention = "attention"
)

// Events lists every event in the order a run reaches them, then the skipped fire, which no run
// reaches because none was started, and the attention alert last, since it can come at any point.
var Events = []string{EventStarted, EventApproval, EventSuccess, EventFailure, EventSkipped,
	EventAttention}

// Object kinds a target can be attached to. A workflow is a template whose steps make a graph, so
// an attachment to one is stored against the template.
const (
	// KindTemplate is a job template or a workflow template.
	KindTemplate = "template"
	// KindSchedule is a schedule.
	KindSchedule = "schedule"
	// KindProject is a project.
	KindProject = "project"
	// KindOrg is an organization.
	KindOrg = "org"
)

// Kinds returns every object kind a target can be attached to, in declaration order. It is the one
// statement of the set. An attachment names its object by kind and id rather than by a foreign key,
// since one table holds attachments to four kinds of object, so no database cascade removes an
// attachment when its object goes. The store contract walks this list instead, on both databases,
// and proves that deleting an object of every kind takes its attachments with it: a kind added
// here without that cleanup fails the build. A source-scan test pins this list to the const block
// above, so it cannot drift from it either.
func Kinds() []string {
	return []string{KindTemplate, KindSchedule, KindProject, KindOrg}
}

// Secret parts a target may be waiting for.
const (
	// PartURL is a target's address, which is the credential for every webhook-shaped channel.
	PartURL = "url"
	// PartKey is a PagerDuty routing key or a Grafana API token.
	PartKey = "key"
)

// kindNeedsURL reports whether a channel kind is addressed by a URL that is stored sealed.
func kindNeedsURL(kind string) bool {
	switch kind {
	case run.NotifyWebhook, run.NotifySlack, run.NotifyMattermost, run.NotifyRocketChat,
		run.NotifyDiscord, run.NotifyTeams, run.NotifyNtfy, run.NotifyGrafana:
		return true
	}
	return false
}

// kindNeedsKey reports whether a channel kind carries a key of its own.
func kindNeedsKey(kind string) bool {
	return kind == run.NotifyPagerDuty || kind == run.NotifyGrafana
}

// Notification is a named notification target, defined once and attached to many objects.
type Notification struct {
	// ID is the unique identifier, prefixed ntf_.
	ID string `json:"id"`
	// Name labels the target for the people attaching it.
	Name string `json:"name"`
	// Description says what the target is for. Optional.
	Description string `json:"description,omitempty"`
	// OrgID is the owning organization, whose members may use the target. Empty is unowned.
	OrgID string `json:"org_id,omitempty"`
	// Kind is the channel: webhook, slack, mattermost, rocketchat, discord, teams, ntfy, pagerduty,
	// grafana, twilio, or email.
	Kind string `json:"kind"`
	// To is the recipient a twilio or email target names. It carries no secret.
	To string `json:"to,omitempty"`
	// URLHint is the target's address masked to its scheme and host, so a reader can tell targets
	// apart without the address, which is the credential for most kinds, ever leaving the server.
	URLHint string `json:"url,omitempty"`
	// KeySet reports that a PagerDuty routing key or a Grafana token is stored.
	KeySet bool `json:"key_set,omitempty"`
	// NeedsSecret reports that the target arrived from an import without the secret it needs, so it
	// delivers nothing until somebody enters it.
	NeedsSecret bool `json:"needs_secret,omitempty"`
	// SealedURL is the sealed address. It never serializes.
	SealedURL string `json:"-"`
	// SealedKey is the sealed routing key or token. It never serializes.
	SealedKey string `json:"-"`
	// CreatedAt is when the target was created.
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy names the actor who created the target, empty for one an import created.
	CreatedBy string `json:"created_by,omitempty"`
}

// Attachment ties a notification target to an object for one event.
type Attachment struct {
	// ID is the unique identifier, prefixed nta_.
	ID string `json:"id"`
	// NotificationID is the target that is told.
	NotificationID string `json:"notification_id"`
	// ObjectKind is what the target is attached to: template, schedule, project, or org.
	ObjectKind string `json:"object_kind"`
	// ObjectID is the id of the object the target is attached to.
	ObjectID string `json:"object_id"`
	// Event is when the target is told: started, success, failure, approval, skipped, or attention.
	Event string `json:"event"`
	// CreatedAt is when the attachment was made.
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy names the actor who made the attachment, empty for one an import made.
	CreatedBy string `json:"created_by,omitempty"`
}

// NewID returns a random notification target identifier prefixed with "ntf_".
func NewID() string {
	return idgen.New("ntf_", 6)
}

// NewAttachmentID returns a random attachment identifier prefixed with "nta_".
func NewAttachmentID() string {
	return idgen.New("nta_", 6)
}

// NormalizeEvent returns the event an attachment names, accepting the names AWX uses for the same
// events: error for failure and approvals for approval. It returns ErrEvent for anything else.
func NormalizeEvent(event string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case EventStarted:
		return EventStarted, nil
	case EventSuccess:
		return EventSuccess, nil
	case EventFailure, "error":
		return EventFailure, nil
	case EventApproval, "approvals":
		return EventApproval, nil
	case EventSkipped:
		return EventSkipped, nil
	case EventAttention:
		return EventAttention, nil
	}
	return "", fmt.Errorf("%w %q: use started, success, failure, approval, skipped, or attention",
		ErrEvent,
		event)
}

// NormalizeObjectKind returns the kind an attachment is stored under, reading a workflow as the
// template it is and an organization by either spelling. It returns ErrObject for anything else.
func NormalizeObjectKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case KindTemplate, "workflow", "job_template", "workflow_job_template":
		return KindTemplate, nil
	case KindSchedule:
		return KindSchedule, nil
	case KindProject:
		return KindProject, nil
	case KindOrg, "organization":
		return KindOrg, nil
	}
	return "", fmt.Errorf("%w, not %q", ErrObject, kind)
}

// EventOf returns the event a run's current state is, or the empty string for a run that is not at
// one, such as a pending run or a child of a split or pipeline.
func EventOf(r *run.Run) string {
	if r == nil || r.ParentID != nil {
		return ""
	}
	switch {
	case r.Kind == run.KindSkippedFire:
		return EventSkipped
	case r.Attention != nil:
		return EventAttention
	case r.Status == run.StatusRunning:
		return EventStarted
	case r.Status == run.StatusPendingApproval:
		return EventApproval
	case r.Status == run.StatusSucceeded:
		return EventSuccess
	case r.Status.Terminal():
		return EventFailure
	}
	return ""
}

// Sealer encrypts and decrypts the secrets a target carries. The credential sealer satisfies it, so
// a target's secrets are sealed under the same key as every credential.
type Sealer interface {
	// Enabled reports whether a key is configured.
	Enabled() bool
	// Seal encrypts plaintext.
	Seal(plaintext string) (string, error)
	// Open decrypts what Seal produced.
	Open(sealed string) (string, error)
}

// SetTarget validates a channel configuration and stores it on n: the kind and recipient as they
// are, the address and key sealed, and a masked hint of the address for display. The plaintext is
// not kept on n. A configuration that carries a secret with no sealer to seal it is refused with
// ErrSealing.
func (n *Notification) SetTarget(t run.NotifyTarget, sealer Sealer) error {
	t.OnFailure = false
	if err := run.ValidateNotifyTarget(t); err != nil {
		return err
	}
	sealedURL, sealedKey := "", ""
	if t.URL != "" || t.Key != "" {
		if sealer == nil || !sealer.Enabled() {
			return ErrSealing
		}
		var err error
		if t.URL != "" {
			if sealedURL, err = sealer.Seal(t.URL); err != nil {
				return fmt.Errorf("seal notification address: %w", err)
			}
		}
		if t.Key != "" {
			if sealedKey, err = sealer.Seal(t.Key); err != nil {
				return fmt.Errorf("seal notification key: %w", err)
			}
		}
	}
	n.Kind, n.To = t.Kind, t.To
	n.SealedURL, n.SealedKey = sealedURL, sealedKey
	n.URLHint = util.MaskURL(t.URL)
	n.KeySet = t.Key != ""
	n.NeedsSecret = false
	return nil
}

// SetKnown stores what is known of a target that is still waiting for its secret: the kind and
// recipient as they are, and whichever of the address and key are present sealed, with a masked
// hint of the address. It is how an import keeps every field it has, such as a Grafana instance's
// address, when the export did not carry the token, so finishing the target asks for the token
// alone. Nothing is validated as complete, because it is not, and the target stays waiting for its
// secret until SetTarget completes it. A part that is present with no sealer to seal it is refused
// with ErrSealing rather than kept in the clear.
func (n *Notification) SetKnown(t run.NotifyTarget, sealer Sealer) error {
	if !run.ValidNotifyKind(t.Kind) {
		return fmt.Errorf("unknown notification kind %q", t.Kind)
	}
	sealedURL, sealedKey := "", ""
	if t.URL != "" || t.Key != "" {
		if sealer == nil || !sealer.Enabled() {
			return ErrSealing
		}
		var err error
		if t.URL != "" {
			if sealedURL, err = sealer.Seal(t.URL); err != nil {
				return fmt.Errorf("seal notification address: %w", err)
			}
		}
		if t.Key != "" {
			if sealedKey, err = sealer.Seal(t.Key); err != nil {
				return fmt.Errorf("seal notification key: %w", err)
			}
		}
	}
	n.Kind, n.To = t.Kind, t.To
	n.SealedURL, n.SealedKey = sealedURL, sealedKey
	if t.URL != "" {
		n.URLHint = util.MaskURL(t.URL)
	}
	n.KeySet = t.Key != ""
	n.NeedsSecret = true
	return nil
}

// Missing names the secret parts a target waiting for its secret still lacks, PartURL, PartKey, or
// both, in that order. It is empty for a target that is configured, and for one whose secret is not
// a part of the target at all. A form finishing an imported target asks for these and nothing else.
func (n *Notification) Missing() []string {
	if n == nil || !n.NeedsSecret {
		return nil
	}
	var out []string
	if kindNeedsURL(n.Kind) && n.SealedURL == "" {
		out = append(out, PartURL)
	}
	if kindNeedsKey(n.Kind) && n.SealedKey == "" {
		out = append(out, PartKey)
	}
	return out
}

// MissingParts names the secret parts a channel configuration lacks for its kind, PartURL, PartKey,
// or both, in that order. A configuration missing none may still be refused for another reason,
// such as a recipient-only kind with no recipient.
func MissingParts(t run.NotifyTarget) []string {
	var out []string
	if kindNeedsURL(t.Kind) && t.URL == "" {
		out = append(out, PartURL)
	}
	if kindNeedsKey(t.Kind) && t.Key == "" {
		out = append(out, PartKey)
	}
	return out
}

// Target opens n's sealed secrets and returns the channel configuration the dispatcher delivers to.
// A target still waiting for its secret returns ErrNeedsSecret.
func (n *Notification) Target(sealer Sealer) (run.NotifyTarget, error) {
	if n.NeedsSecret {
		return run.NotifyTarget{}, ErrNeedsSecret
	}
	return n.Known(sealer)
}

// Known opens whatever n stores, whether or not it is still waiting for its secret, and returns it
// as a channel configuration. It is for completing a target, where the parts already stored are
// kept and only the missing one is asked for, and never for delivery, which uses Target.
func (n *Notification) Known(sealer Sealer) (run.NotifyTarget, error) {
	t := run.NotifyTarget{Kind: n.Kind, To: n.To}
	if n.SealedURL == "" && n.SealedKey == "" {
		return t, nil
	}
	if sealer == nil || !sealer.Enabled() {
		return run.NotifyTarget{}, ErrSealing
	}
	var err error
	if n.SealedURL != "" {
		if t.URL, err = sealer.Open(n.SealedURL); err != nil {
			return run.NotifyTarget{}, fmt.Errorf("open notification address: %w", err)
		}
	}
	if n.SealedKey != "" {
		if t.Key, err = sealer.Open(n.SealedKey); err != nil {
			return run.NotifyTarget{}, fmt.Errorf("open notification key: %w", err)
		}
	}
	return t, nil
}

// Clone returns a copy, so a caller cannot change stored state through what a store handed it.
func (n *Notification) Clone() *Notification {
	if n == nil {
		return nil
	}
	out := *n
	return &out
}

// Clone returns a copy, so a caller cannot change stored state through what a store handed it.
func (a *Attachment) Clone() *Attachment {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// Store persists notification targets and their attachments. Implementations must be safe for
// concurrent use.
type Store interface {
	// Save inserts or replaces the target identified by n.ID.
	Save(ctx context.Context, n *Notification) error
	// Update replaces an existing target, or returns ErrNotFound when it is gone, so an edit racing
	// a delete cannot re-create what was deleted.
	Update(ctx context.Context, n *Notification) error
	// Get returns the target with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Notification, error)
	// List returns every target ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Notification, error)
	// Delete removes the target and every attachment it has, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// Attach stores an attachment, or returns ErrDuplicate when the same target is already attached
	// to the same object for the same event.
	Attach(ctx context.Context, a *Attachment) error
	// Detach removes the attachment with the given id, or returns ErrAttachmentNotFound.
	Detach(ctx context.Context, id string) error
	// Attachments returns a target's attachments ordered by creation time, oldest first.
	Attachments(ctx context.Context, notificationID string) ([]*Attachment, error)
	// AttachedTo returns the attachments on one object, ordered by creation time, oldest first.
	AttachedTo(ctx context.Context, kind, objectID string) ([]*Attachment, error)
	// ListAttachments returns every attachment, ordered by creation time, oldest first. The doctor
	// reads it to find an attachment whose object or target is gone, from whatever path it came.
	ListAttachments(ctx context.Context) ([]*Attachment, error)
	// DetachObject removes every attachment on one object and returns a summary of what it
	// removed. It is the cleanup for a store whose objects are kept somewhere a delete cannot
	// reach in the same transaction; the database stores remove attachments inside the object's
	// own delete instead.
	DetachObject(ctx context.Context, kind, objectID string) (Cleanup, error)

	// Record appends ev to its run's sequence of notification events and queues one delivery to
	// each recipient, in one transaction, assigning ev the run's next sequence number. An event the
	// run already holds under the same DedupeKey is not recorded again and Record reports false, so
	// a moment announced twice, by a retry or by two processes, is delivered once. An event Once
	// names is recorded once per run and branch whatever its snapshot says: the run's end, its
	// start, its hold, and each approval step's hold. So one announced by the process that moved
	// the run and again by the sweep that found it owed reaches each target once.
	Record(ctx context.Context, ev *RunEvent, recipients []Recipient) (bool, error)
	// Claim leases up to limit deliveries to owner until now plus lease, and returns them with the
	// run snapshot each one delivers. A delivery is claimable when it is pending, due by now, not
	// held under an unexpired claim, and every earlier delivery to the same target for the same run
	// has finished, except one on a different branch of a workflow, which is concurrent with it. A
	// delivery that is claimed by nobody else can be taken by exactly one claimant. When perTarget
	// is above zero, owner is given at most that many deliveries to one target at once, counting
	// the ones it already holds under unexpired claims, so a target whose attempts hang occupies
	// only its own share of a claimant's attempts and every other target's deliveries are still
	// claimed.
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration,
		limit, perTarget int) ([]*Delivery, error)
	// Finish records what an attempt at a claimed delivery came to and releases the claim, or
	// returns ErrDeliveryLost when owner no longer holds it. DeliveryDelivered, DeliveryFailed,
	// and DeliverySkipped finish the delivery.
	Finish(ctx context.Context, d *Delivery, owner string, out Outcome) error
	// Release lets go of owner's claim on a delivery whose attempt ended because the claimant was
	// stopping, not because the target answered. No attempt is counted and the delivery stays due
	// as it was, so the next process to claim it makes the attempt the stop cut short. It returns
	// ErrDeliveryLost when owner no longer holds the delivery.
	Release(ctx context.Context, d *Delivery, owner string) error
	// Deliveries lists the deliveries a filter selects, newest first.
	Deliveries(ctx context.Context, f DeliveryFilter) ([]*Delivery, error)
}
