package dispatch

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// RunDecision is one decision on a run held for approval.
type RunDecision struct {
	// Approve is true to release the run and false to reject it.
	Approve bool
	// Reason is the approver's stated reason as submitted, empty for none. It is masked before
	// anything records it, and the text as submitted is never stored.
	Reason string
	// ConfirmedMask is the masked reason the approver was shown and confirmed. When the masker
	// changes a reason, the decision is refused with a ReasonMaskedError, recording nothing, until
	// this equals the masked text.
	ConfirmedMask string
	// By names the decider.
	By outcome.Decider
}

// ReasonMaskedError is returned when the secret masker changed an approver's reason and the
// approver has not confirmed the masked text. Nothing is recorded: the approver is shown Masked,
// and the decision goes ahead when it is sent again confirming exactly that text. A reason is never
// refused for looking secret, only held until the person who wrote it has seen what will be kept.
type ReasonMaskedError struct {
	// Masked is the reason as it would be stored.
	Masked string
}

// Error says what the approver has to do.
func (e *ReasonMaskedError) Error() string {
	return "the reason was masked for known secrets before storage: confirm the masked text to " +
		"record it"
}

// Correction is a correction appended to a decision's reason.
type Correction struct {
	// Text is the correction as submitted. It is masked before anything records it.
	Text string
	// ConfirmedMask is the masked text the writer was shown and confirmed.
	ConfirmedMask string
	// By names who writes it.
	By outcome.Decider
}

// DecideRun approves or rejects a run held for approval, recording the approver's reason with the
// decision when one is given. It is Approve and Reject with a reason for either.
func (d *Dispatcher) DecideRun(ctx context.Context, id string, dec RunDecision) (*run.Run, error) {
	if dec.Approve {
		return d.approveRun(ctx, id, dec)
	}
	return d.rejectRun(ctx, id, dec)
}

// Decisions returns the decision records of a run, its approval steps' included, in thread order:
// the decisions that took effect, each the one its run or step stores as its winner, and the
// corrections appended to them.
func (d *Dispatcher) Decisions(ctx context.Context, runID string) ([]*decision.Record, error) {
	records, err := d.decisions.ForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return outcome.EffectiveDecisions(ctx, d.store, records)
}

// DecisionStore returns the store decision records are kept in, so a receipt or a dossier built
// beside this dispatcher reads the same records the decisions were made with.
func (d *Dispatcher) DecisionStore() decision.Store {
	return d.decisions
}

// prepareReason canonicalizes, caps, and masks a reason, and reports the masked text and whether
// the masker changed it. An empty result is no reason. A reason the masker changed that the
// approver has not confirmed is refused with a ReasonMaskedError, so nothing is recorded until they
// have seen it.
func (d *Dispatcher) prepareReason(ctx context.Context, subject *run.Run, text,
	confirmed string) (string, bool, error) {
	canonical := decision.Canonical(text)
	if canonical == "" {
		return "", false, nil
	}
	if decision.TooLong(canonical) {
		return "", false, fmt.Errorf("%w: a reason holds at most %d characters", ErrReasonTooLong,
			decision.MaxReasonChars)
	}
	masked := decision.Canonical(d.maskReason(ctx, subject, canonical))
	if masked == canonical {
		return masked, false, nil
	}
	if confirmed != masked {
		return "", false, &ReasonMaskedError{Masked: masked}
	}
	return masked, true, nil
}

// maskReason runs the secret masker over a reason: every secret the run being decided could have
// shown the approver, its own variables and command, each workflow step's command, its stored
// credentials and inventory, and then secret-looking assignments of any kind. It is the reading run
// output and pull request comments get, so a value masked there is masked here.
func (d *Dispatcher) maskReason(ctx context.Context, subject *run.Run, text string) string {
	masked := d.RedactRunText(ctx, subject, text)
	var values []string
	for _, step := range subject.Steps {
		values = append(values, runOwnSecrets(nil, step.Command)...)
	}
	if len(values) == 0 {
		return masked
	}
	m := &masker{}
	m.set(values)
	return m.redactString(masked)
}

// newDecisionRecord builds the record of a decision on subject, the run decided on or, for an
// approval step, the workflow. step is the approval step's record, nil for a whole run. The
// record's id is minted here and becomes the id of the chain entry that commits it, so each names
// the other.
func (d *Dispatcher) newDecisionRecord(subject, step *run.Run, verdict, reason string, masked bool,
	by outcome.Decider) (*decision.Record, error) {
	id := audit.NewID()
	rec := &decision.Record{
		ID: id, Kind: decision.KindDecision, DecisionID: id, RunID: subject.ID, Verdict: verdict,
		At: d.now(), Actor: by.Name, ActorType: by.Type, OnBehalfOf: by.OnBehalfOf,
	}
	held := subject
	if step != nil {
		rec.StepRunID = step.ID
		held = step
	}
	if reason != "" {
		random, commitment, err := decision.Commit(id, reason)
		if err != nil {
			return nil, err
		}
		rec.Reason = &decision.Reason{Text: reason, Random: random, Commitment: commitment,
			Masked: masked}
	}
	// An agent's run records how separation of duties applied to whoever decided it. The account the
	// agent is bound to counts as the requester, and the comparison is by account where both sides
	// name one, the same comparison that refused a self-approval before this was reached.
	if subject.Initiator != nil {
		account := by.OnBehalfOf
		if account == "" {
			account = by.Name
		}
		rec.SeparationOfDuties = decision.Evaluate(held.RequireDistinctApprover,
			subject.Initiator.BoundTo, account, sameRequester(by, held), verdict == "approved")
	}
	return rec, nil
}

// keepDecision stores a decision record ahead of the chain entry that commits it, so the reason's
// text and random value exist before anything is committed to them. commit appends the entry; when
// it fails the record is withdrawn, so no record stands for a decision the chain does not hold.
func (d *Dispatcher) keepDecision(ctx context.Context, rec *decision.Record,
	commit func(outcome.DecisionExtras) error) error {
	if err := d.decisions.Save(ctx, rec); err != nil {
		return fmt.Errorf("keep the decision record: %w", err)
	}
	if commit == nil {
		return nil
	}
	if err := commit(outcome.ExtrasOf(rec)); err != nil {
		if derr := d.decisions.Delete(context.Background(), rec.ID); derr != nil {
			d.log.Error("dispatch: withdraw a decision record the chain does not hold: "+derr.Error(),
				zap.String("decision_id", rec.ID))
		}
		return err
	}
	return nil
}

// requireReason refuses a decision that carries no reason when the rule that held the run asks for
// one, naming the rule so the approver knows what asked.
func requireReason(held *run.Run, reason string, approve bool) error {
	if reason != "" || !decision.Required(held.RequireReason, approve) {
		return nil
	}
	rule := held.HeldByPolicy
	if rule == "" {
		rule = "the rule that held it"
	}
	what := "a denial"
	if approve {
		what = "an approval"
	}
	return fmt.Errorf("%w: %s requires a reason for %s of this run", ErrReasonRequired, rule, what)
}

// AddCorrection appends a correction to a decision's reason. A reason is never edited: the
// correction is a record of its own, masked the way a reason is, committed by its own chain entry
// that names the decision it corrects, and shown with it as a thread.
func (d *Dispatcher) AddCorrection(ctx context.Context, runID, decisionID string,
	c Correction) (*decision.Record, error) {
	if c.By.Type == agentActorType {
		return nil, fmt.Errorf("%w or annotate a decision", ErrAgentApproval)
	}
	original, err := d.decisions.Get(ctx, decisionID)
	if errors.Is(err, decision.ErrNotFound) || (err == nil && (original.RunID != runID ||
		original.Kind != decision.KindDecision)) {
		return nil, ErrDecisionNotFound
	}
	if err != nil {
		return nil, err
	}
	subject, err := d.store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	text, masked, err := d.prepareReason(ctx, subject, c.Text, c.ConfirmedMask)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, ErrNoReasonText
	}
	id := audit.NewID()
	random, commitment, err := decision.Commit(id, text)
	if err != nil {
		return nil, err
	}
	rec := &decision.Record{
		ID: id, Kind: decision.KindCorrection, DecisionID: original.ID, RunID: runID,
		StepRunID: original.StepRunID, At: d.now(), Actor: c.By.Name, ActorType: c.By.Type,
		OnBehalfOf: c.By.OnBehalfOf,
		Reason:     &decision.Reason{Text: text, Random: random, Commitment: commitment, Masked: masked},
	}
	var commit func(outcome.DecisionExtras) error
	if d.audits != nil {
		commit = func(outcome.DecisionExtras) error {
			return outcome.CommitCorrection(ctx, d.audits, rec, c.By, d.now)
		}
	}
	if err := d.keepDecision(ctx, rec, commit); err != nil {
		return nil, fmt.Errorf("record the correction: %w", err)
	}
	return rec, nil
}

// RedactReason removes a reason's text and random value together, an explicit privacy action
// rather than an edit. The redaction is recorded on the chain, naming who, when, and the category
// and referencing the decision, so a reason never disappears without a record that it did. The
// commitment stays on the chain and in the record, and can never be opened again. Receipts already
// issued keep what they disclosed.
//
// The redaction claims the record before anything is recorded: one conditional update removes the
// text and stores the redaction, its chain entry's id fixed and marked pending. Of two redactions
// racing, exactly one removes the text, and only that one is ever recorded, so the chain never
// holds two removals under two causes for one reason. The entry is then appended once under its id
// and the mark cleared. A redaction whose append fails, or whose process dies first, stays pending
// with the text already gone, and the next attempt or the janitor records it. The update used to
// follow the entry, so a failed update left a chain saying the text was removed while every receipt
// drawn afterward still disclosed it.
func (d *Dispatcher) RedactReason(ctx context.Context, runID, recordID, category string,
	by outcome.Decider) (*decision.Record, error) {
	if by.Type == agentActorType {
		return nil, fmt.Errorf("%w or annotate a decision", ErrAgentApproval)
	}
	if !decision.ValidCategory(category) {
		return nil, fmt.Errorf("%w: category must be %s, %s, or %s", ErrRedactionCategory,
			decision.CategoryPersonalData, decision.CategorySecret, decision.CategoryOther)
	}
	rec, err := d.decisions.Get(ctx, recordID)
	if errors.Is(err, decision.ErrNotFound) || (err == nil && rec.RunID != runID) {
		return nil, ErrDecisionNotFound
	}
	if err != nil {
		return nil, err
	}
	if !rec.HasReason() {
		return nil, decision.ErrNoReason
	}
	if rec.Reason.Redacted != nil {
		// Another redaction holds the record. A pending one is finished here, which is how a
		// retry after a failure records it, and this one is refused either way.
		d.finishHeldRedaction(ctx, rec)
		return nil, decision.ErrRedacted
	}
	red := decision.Redaction{At: d.now(), Actor: by.Name, ActorType: by.Type,
		OnBehalfOf: by.OnBehalfOf, Category: category, EntryID: audit.NewID(),
		Pending: d.audits != nil}
	if err := d.decisions.Redact(ctx, recordID, red); err != nil {
		if errors.Is(err, decision.ErrRedacted) {
			if cur, gerr := d.decisions.Get(ctx, recordID); gerr == nil {
				d.finishHeldRedaction(ctx, cur)
			}
			return nil, err
		}
		// The update may have landed with its answer lost. The record says whether this
		// redaction holds it.
		cur, gerr := d.decisions.Get(ctx, recordID)
		if gerr != nil || !cur.HasReason() || cur.Reason.Redacted == nil ||
			cur.Reason.Redacted.EntryID != red.EntryID {
			return nil, err
		}
	}
	if red.Pending {
		claimed := rec.Clone()
		claimed.Reason.Redacted = &red
		if err := d.finishRedaction(ctx, claimed); err != nil {
			return nil, fmt.Errorf("%w: %w", errRedactionUnrecorded, err)
		}
	}
	return d.decisions.Get(ctx, recordID)
}

// errRedactionUnrecorded marks a redaction that removed the text and could not yet be recorded on
// the chain. It stays pending, and the janitor records it once the chain accepts the entry.
var errRedactionUnrecorded = errors.New("the reason is removed and its redaction is not yet " +
	"recorded: it is recorded once the audit trail accepts it")

// finishHeldRedaction finishes the redaction a record holds when it is still pending, logging what
// it could not finish, since the janitor tries again.
func (d *Dispatcher) finishHeldRedaction(ctx context.Context, rec *decision.Record) {
	if !rec.HasReason() || rec.Reason.Redacted == nil || !rec.Reason.Redacted.Pending {
		return
	}
	if err := d.finishRedaction(ctx, rec); err != nil {
		d.log.Error("dispatch: finish a pending reason redaction: "+err.Error(),
			zap.String("decision_id", rec.ID))
	}
}

// finishRedaction appends a pending redaction's chain entry, once under its fixed id, and clears
// the pending mark. Any number of finishers may run at once: the entry is refused as a duplicate
// for all but the first, and clearing a mark already cleared is not an error.
func (d *Dispatcher) finishRedaction(ctx context.Context, rec *decision.Record) error {
	red := *rec.Reason.Redacted
	if d.audits != nil {
		err := withRetries(func() error {
			aerr := d.audits.Append(ctx, outcome.RedactionEntry(rec, red, d.now))
			if errors.Is(aerr, audit.ErrDuplicateID) {
				return nil
			}
			return aerr
		})
		if err != nil {
			return fmt.Errorf("record the redaction: %w", err)
		}
	}
	return withRetries(func() error {
		return d.decisions.FinishRedaction(ctx, rec.ID, red.EntryID)
	})
}

// finishPendingRedactions records every redaction a process claimed and did not record, on every
// replica's janitor.
func (d *Dispatcher) finishPendingRedactions() {
	pending, err := d.decisions.PendingRedactions(d.ctx)
	if err != nil {
		if d.ctx.Err() == nil {
			d.log.Error("dispatch: list pending reason redactions: " + err.Error())
		}
		return
	}
	for _, rec := range pending {
		d.finishHeldRedaction(d.ctx, rec)
	}
}
