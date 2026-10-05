package outcome

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
)

// DecisionExtras is what a decision recorded with a decision record commits beside its verdict and
// its spec or state digest: the record's id, the commitment to the approver's reason, and, for a
// run an agent asked for, the separation-of-duties evaluation. Each is omitted from the body when
// empty.
type DecisionExtras struct {
	// ID is the decision record's id, used as the committing chain entry's id. Empty mints a fresh
	// entry id and commits no record id, which is the shape of a decision recorded without a record.
	ID string
	// ReasonCommitment is the commitment to the approver's masked reason, empty when none was given.
	ReasonCommitment string
	// SeparationOfDuties is the evaluation for an agent-initiated run, nil otherwise.
	SeparationOfDuties *decision.SeparationOfDuties
	// Comment is the pull request comment the decision was made from, nil otherwise.
	Comment *decision.Comment
}

// ExtrasOf returns what a decision record commits in its decision's body.
func ExtrasOf(rec *decision.Record) DecisionExtras {
	if rec == nil {
		return DecisionExtras{}
	}
	extras := DecisionExtras{ID: rec.DecisionID, SeparationOfDuties: rec.SeparationOfDuties,
		Comment: rec.Comment}
	if rec.Reason != nil {
		extras.ReasonCommitment = rec.Reason.Commitment
	}
	return extras
}

// entryID returns id when a caller chose one and a fresh audit entry id otherwise.
func entryID(id string) string {
	if id != "" {
		return id
	}
	return audit.NewID()
}

// CorrectionRecord is the canonical body a correction's chain entry commits: which run and decision
// it corrects, its own id, and the commitment to its text. Like a decision's reason, the text
// itself is never in the body.
type CorrectionRecord struct {
	// RunID is the run the corrected decision was made on.
	RunID string `json:"run_id"`
	// StepRunID is the workflow approval step decided on, omitted for a decision on a whole run.
	StepRunID string `json:"step_run_id,omitempty"`
	// DecisionID is the decision corrected.
	DecisionID string `json:"decision_id"`
	// CorrectionID is the correction's own record id, the event id its commitment is bound to.
	CorrectionID string `json:"correction_id"`
	// ReasonCommitment is the hiding commitment to the correction's masked text.
	ReasonCommitment string `json:"reason_commitment"`
}

// CorrectionBody assembles the canonical correction record for rec.
func CorrectionBody(rec *decision.Record) ([]byte, error) {
	out := CorrectionRecord{RunID: rec.RunID, StepRunID: rec.StepRunID, DecisionID: rec.DecisionID,
		CorrectionID: rec.ID}
	if rec.Reason != nil {
		out.ReasonCommitment = rec.Reason.Commitment
	}
	return json.Marshal(out)
}

// CommitCorrection records a correction as a chain entry naming who wrote it and committing its
// body, under the correction record's own id. It is appended before the correction is shown
// anywhere, fail-closed, as a decision is.
func CommitCorrection(ctx context.Context, audits audit.Store, rec *decision.Record, by Decider,
	now func() time.Time) error {
	body, err := CorrectionBody(rec)
	if err != nil {
		return err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return err
	}
	if now == nil {
		now = time.Now
	}
	return audits.Append(ctx, &audit.Entry{
		ID: rec.ID, At: now(),
		Actor: by.Name, ActorType: by.Type, OnBehalfOf: by.OnBehalfOf,
		Method: audit.MethodReason, Path: decision.CorrectionPath(rec.RunID, rec.DecisionID, rec.ID),
		ContentDigest: digest, Nonce: nonce,
	})
}

// CommitRedaction records that a reason's text and random value are being removed: who, when, and
// the category, referencing the decision and, for a correction, the correction. It carries no body,
// since everything it says is in its path, and it never names what the text held. It returns the
// entry's id.
func CommitRedaction(ctx context.Context, audits audit.Store, rec *decision.Record, category string,
	by Decider, now func() time.Time) (string, error) {
	entry := RedactionEntry(rec, decision.Redaction{Category: category, Actor: by.Name,
		ActorType: by.Type, OnBehalfOf: by.OnBehalfOf}, now)
	if err := audits.Append(ctx, entry); err != nil {
		return "", err
	}
	return entry.ID, nil
}

// RedactionEntry builds the chain entry that records red, a redaction of rec's reason, without
// appending it. Its id is red's EntryID when one is fixed, a fresh id otherwise, and its time is
// red's when red carries one. A redaction claims its record before it is recorded and carries this
// entry's id in the claim, so whoever finishes the redaction appends exactly this entry, once.
func RedactionEntry(rec *decision.Record, red decision.Redaction, now func() time.Time) *audit.Entry {
	if now == nil {
		now = time.Now
	}
	correction := ""
	if rec.Kind == decision.KindCorrection {
		correction = rec.ID
	}
	at := red.At
	if at.IsZero() {
		at = now()
	}
	return &audit.Entry{
		ID: entryID(red.EntryID), At: at,
		Actor: red.Actor, ActorType: red.ActorType, OnBehalfOf: red.OnBehalfOf,
		Method: audit.MethodReason,
		Path:   decision.RedactionPath(rec.RunID, rec.DecisionID, correction, red.Category),
	}
}
