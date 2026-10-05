// Package decision keeps what an approval decision carries beyond the chain entry that commits it:
// the reason the approver gave, the random value that hides it, the corrections appended to it,
// and, for a run an agent asked for, how separation of duties was evaluated.
//
// A reason is audit evidence, so it is committed to the chain, but it is also free text a person
// typed, so it must be possible to remove it later without breaking the chain. The chain therefore
// holds a hiding commitment to the reason and never the reason itself: SHA-256 over the canonical
// masked text, a random 32-byte value, and the id of the decision it belongs to. The text and the
// random value are stored here, beside each other, and disclosed together in the dossier and the
// receipt, which is what lets anybody holding them check the commitment offline with no secret.
// Removing both is a redaction: the commitment stays on the chain and can never be opened again.
package decision

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kordloom/loomseal/jcs"
)

// Kinds of record. A decision is an approval or a denial made by a person, and a correction is a
// note appended to one afterward, since a reason is never edited.
const (
	// KindDecision marks the record of an approval or a denial.
	KindDecision = "decision"
	// KindCorrection marks a correction appended to a decision's reason.
	KindCorrection = "correction"
)

// Redaction categories say why a reason was removed, without saying what it held.
const (
	// CategoryPersonalData is a reason removed because it held personal data, for example after an
	// erasure request.
	CategoryPersonalData = "personal_data"
	// CategorySecret is a reason removed because it held a secret the masker did not recognize.
	CategorySecret = "secret"
	// CategoryOther is a reason removed for any other stated cause.
	CategoryOther = "other"
)

// Separation-of-duties results recorded on a decision about an agent-initiated run.
const (
	// SoDSatisfied is an approval a rule required an independent approver for, given by one.
	SoDSatisfied = "satisfied"
	// SoDNotRequired is an approval no rule required an independent approver for.
	SoDNotRequired = "not_required"
	// SoDNotApplicable is a denial, which separation of duties never restricts: withdrawing a
	// request needs nobody else.
	SoDNotApplicable = "not_applicable"
)

// Reason requirements a rule can copy onto the runs it holds.
const (
	// RequireDenials requires a reason on a denial.
	RequireDenials = "denials"
	// RequireAlways requires a reason on an approval and on a denial.
	RequireAlways = "always"
)

// MaxReasonChars is the most characters a reason or a correction may hold.
const MaxReasonChars = 1000

// randomBytes is the size of the random value a commitment hides its reason under.
const randomBytes = 32

// Record is one decision made by a person on a held run or a workflow approval step, or one
// correction appended to such a decision. Its id is the id of the chain entry that committed it.
type Record struct {
	// ID is the id of the chain entry that recorded this decision or correction.
	ID string `json:"id"`
	// Kind is decision or correction.
	Kind string `json:"kind"`
	// DecisionID is the decision this record belongs to: its own id for a decision, the corrected
	// decision's id for a correction.
	DecisionID string `json:"decision_id"`
	// RunID is the run decided on, or for a workflow approval step, the workflow run.
	RunID string `json:"run_id"`
	// StepRunID is the workflow approval step's record, empty for a decision on a whole run.
	StepRunID string `json:"step_run_id,omitempty"`
	// Verdict is approved or rejected for a decision, empty for a correction.
	Verdict string `json:"verdict,omitempty"`
	// At is when it was recorded.
	At time.Time `json:"at"`
	// Actor names who made it, exactly as the chain entry names them.
	Actor string `json:"actor"`
	// ActorType is how they authenticated, in the chain's vocabulary.
	ActorType string `json:"actor_type,omitempty"`
	// OnBehalfOf is the account whose authority they used.
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	// Reason is the reason given with it, nil when none was.
	Reason *Reason `json:"reason,omitempty"`
	// SeparationOfDuties is how separation of duties was evaluated for a decision on a run an agent
	// asked for, nil for every other record.
	SeparationOfDuties *SeparationOfDuties `json:"separation_of_duties,omitempty"`
	// Comment is the pull request comment the decision was made from, nil for a decision made any
	// other way.
	Comment *Comment `json:"comment,omitempty"`
}

// Comment identifies the pull request or merge request comment a decision was made from. It names
// the comment and its author by the forge's numeric ids and fixes the body by its SHA-256, so an
// edit to the comment or its deletion afterward cannot change what the decision records.
type Comment struct {
	// Forge is github or gitlab.
	Forge string `json:"forge"`
	// APIURL is the REST API base of the forge the comment is on.
	APIURL string `json:"api_url"`
	// Repository is the repository the pull request belongs to: owner/name or group/project.
	Repository string `json:"repository"`
	// PullRequest is the pull request number on GitHub, the merge request iid on GitLab.
	PullRequest int `json:"pull_request"`
	// CommentID is the forge's numeric id of the comment.
	CommentID int64 `json:"comment_id"`
	// AuthorID is the forge's numeric id of the account that wrote the comment.
	AuthorID int64 `json:"author_id"`
	// BodySHA256 is the hex SHA-256 of the comment body as the forge holds it, which the webhook's
	// copy was required to match.
	BodySHA256 string `json:"body_sha256"`
	// PlanRunID is the plan run whose saved plan the comment approved applying.
	PlanRunID string `json:"plan_run_id,omitempty"`
}

// Reason is an approver's stated reason, as stored and as disclosed.
type Reason struct {
	// Text is the canonical masked reason, empty once redacted.
	Text string `json:"text,omitempty"`
	// Random is the hex of the random value the commitment hides the text under, empty once
	// redacted. It is not a secret: it is disclosed beside the text so the commitment can be
	// checked, and removing it with the text is what makes a redaction permanent.
	Random string `json:"random,omitempty"`
	// Commitment is what the chain committed: "sha256:" and the hex of the commitment digest.
	Commitment string `json:"commitment"`
	// Masked reports that the secret masker changed the text before it was stored, which the
	// approver was shown and confirmed.
	Masked bool `json:"masked,omitempty"`
	// Redacted records the redaction that removed the text, nil while the text is held.
	Redacted *Redaction `json:"redacted,omitempty"`
}

// Redaction records that a reason's text and random value were removed together.
type Redaction struct {
	// At is when it was redacted.
	At time.Time `json:"at"`
	// Actor names who redacted it, as the chain entry recording the redaction names them.
	Actor string `json:"actor"`
	// ActorType is how they authenticated.
	ActorType string `json:"actor_type,omitempty"`
	// OnBehalfOf is the account whose authority they used.
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	// Category says why, without saying what the text held.
	Category string `json:"category"`
	// EntryID is the chain entry that records the redaction, its id fixed before the text is
	// removed so that whoever finishes the redaction appends it once.
	EntryID string `json:"entry_id,omitempty"`
	// Pending marks a redaction whose text is already removed and whose chain entry is not yet
	// known to be on the chain. The redaction claims the record first, so of two redactions racing
	// exactly one removes the text and only that one is ever recorded. Whoever finishes it, the
	// process that made it, a retry, or the janitor after a crash, appends EntryID once and clears
	// this.
	Pending bool `json:"pending,omitempty"`
}

// SeparationOfDuties is how separation of duties was evaluated when a person decided on a run an
// agent asked for. The account the agent is bound to counts as the requester.
type SeparationOfDuties struct {
	// Required says a rule required an independent approver.
	Required bool `json:"required"`
	// Requester is the account that counts as the requester: the account the agent is bound to.
	Requester string `json:"requester"`
	// Decider is the account the decider used.
	Decider string `json:"decider"`
	// Independent says the decider is not the requester.
	Independent bool `json:"independent"`
	// Result is satisfied, not_required, or not_applicable.
	Result string `json:"result"`
}

// HasReason reports whether the record was given a reason, held or redacted.
func (r *Record) HasReason() bool {
	return r != nil && r.Reason != nil && r.Reason.Commitment != ""
}

// Clone returns a deep copy of r, nil for nil.
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	out := *r
	if r.Reason != nil {
		reason := *r.Reason
		if r.Reason.Redacted != nil {
			red := *r.Reason.Redacted
			reason.Redacted = &red
		}
		out.Reason = &reason
	}
	if r.SeparationOfDuties != nil {
		sod := *r.SeparationOfDuties
		out.SeparationOfDuties = &sod
	}
	if r.Comment != nil {
		c := *r.Comment
		out.Comment = &c
	}
	return &out
}

// Canonical reduces a reason to the form that is masked, stored, committed, and disclosed: line
// breaks normalized to a line feed, control characters other than a line feed and a tab removed,
// the bidirectional override and isolate characters removed, and surrounding whitespace trimmed. A
// disclosed reason is already canonical, so a verifier commits to it exactly as disclosed and never
// needs this function.
//
// Those characters are removed rather than refused. A reason is never refused for what it looks
// like, and a stray escape sequence or a direction override in an audit record is a way to make the
// terminal or the page that shows it display something other than what was recorded.
func Canonical(text string) string {
	text = strings.ToValidUTF8(text, "�")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), bidiControl(r):
			return -1
		}
		return r
	}, text)
	return strings.TrimSpace(text)
}

// bidiControl reports whether r is a bidirectional embedding, override, or isolate character, which
// reorders how the text around it is displayed without being visible itself.
func bidiControl(r rune) bool {
	return (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩')
}

// TooLong reports whether a canonical reason is over the cap, counted in characters.
func TooLong(text string) bool {
	return utf8.RuneCountInString(text) > MaxReasonChars
}

// Commit hides text under a fresh random value bound to the decision event id, and returns the
// random value's hex and the commitment. text must already be canonical and masked: the commitment
// is over exactly what is stored and disclosed, never the text as it was typed.
func Commit(eventID, text string) (random, commitment string, err error) {
	var raw [randomBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrRandom, err)
	}
	random = hex.EncodeToString(raw[:])
	commitment, err = Commitment(eventID, random, text)
	if err != nil {
		return "", "", err
	}
	return random, commitment, nil
}

// Commitment computes the commitment to text under random for the decision event id: "sha256:" and
// the hex SHA-256 of the RFC 8785 canonical JSON object {"event": eventID, "random": random,
// "reason": text}. The canonical object makes the three inputs unambiguous without a length prefix
// or a separator, and it is the canonicalization every LoomSeal verifier already carries, so a
// third party can recompute it in any language from what a receipt discloses.
//
// It is not keyed by anything the server holds. The random value is what hides the text: without it
// the commitment cannot be guessed back to the text, and with it, disclosed beside the text,
// anybody can check it.
func Commitment(eventID, random, text string) (string, error) {
	raw, err := hex.DecodeString(random)
	if err != nil || len(raw) != randomBytes {
		return "", fmt.Errorf("%w: the random value must be %d bytes of hex", ErrCommitment, randomBytes)
	}
	if eventID == "" {
		return "", fmt.Errorf("%w: a commitment is bound to a decision event id", ErrCommitment)
	}
	canonical, err := jcs.Serialize(map[string]any{"event": eventID, "random": random,
		"reason": text})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrCommitment, err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Verify reports whether text under random opens commitment for the decision event id.
func Verify(commitment, eventID, random, text string) bool {
	got, err := Commitment(eventID, random, text)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(commitment)) == 1
}

// ValidCategory reports whether c names a redaction category.
func ValidCategory(c string) bool {
	switch c {
	case CategoryPersonalData, CategorySecret, CategoryOther:
		return true
	}
	return false
}

// ValidRequirement reports whether v names a reason requirement a rule may set, the empty string
// meaning none.
func ValidRequirement(v string) bool {
	switch v {
	case "", RequireDenials, RequireAlways:
		return true
	}
	return false
}

// Stricter returns the stricter of two reason requirements, so rules that cover the same run
// compose the way separation of duties does: if any of them asks for a reason, the run asks for
// one.
func Stricter(a, b string) string {
	rank := func(v string) int {
		switch v {
		case RequireAlways:
			return 2
		case RequireDenials:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Required reports whether a decision with the given verdict needs a reason under requirement.
func Required(requirement string, approve bool) bool {
	switch requirement {
	case RequireAlways:
		return true
	case RequireDenials:
		return !approve
	}
	return false
}

// Evaluate records how separation of duties applied to a decision on a run an agent asked for.
// requester is the account the agent is bound to, decider the account the decider used, and same
// whether those are one account, which the caller decides by account id where it has one. An
// approval that a rule required to be independent and was not never reaches here: it is refused
// before anything is recorded.
func Evaluate(required bool, requester, decider string, same, approve bool) *SeparationOfDuties {
	sod := &SeparationOfDuties{Required: required, Requester: requester, Decider: decider,
		Independent: !same}
	switch {
	case !approve:
		sod.Result = SoDNotApplicable
	case required:
		sod.Result = SoDSatisfied
	default:
		sod.Result = SoDNotRequired
	}
	return sod
}
