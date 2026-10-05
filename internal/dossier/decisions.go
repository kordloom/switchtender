package dossier

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// AttachDecisions adds the run's decision records to the collected evidence: each approval and
// denial a person made, the reason given with it and the random value its chain commitment hides it
// under, the corrections appended to it, any redaction, and for an agent's run the
// separation-of-duties evaluation. A nil store attaches nothing, which is an install that keeps no
// records. Only decisions that took effect are attached, read against runs: the decision the run,
// or its approval step, stores as the one that won it.
func AttachDecisions(ctx context.Context, in *Input, store decision.Store, runs run.Store) error {
	if in == nil || in.Run == nil || store == nil {
		return nil
	}
	records, err := store.ForRun(ctx, in.Run.ID)
	if err != nil {
		return fmt.Errorf("read decision records: %w", err)
	}
	if runs != nil {
		if records, err = outcome.EffectiveDecisions(ctx, runs, records); err != nil {
			return fmt.Errorf("read decision records: %w", err)
		}
	}
	in.Decisions = records
	return nil
}

// reasonRow is one decision or correction as the dossier renders it.
type reasonRow struct {
	// Kind is Approved, Rejected, or Correction.
	Kind string
	// Actor is who made it, with the account they acted for.
	Actor string
	// At is when, formatted.
	At string
	// Step names the workflow approval step decided, empty for a whole run.
	Step string
	// Reason is the reason's text, empty when none was given or it was redacted.
	Reason string
	// Masked says the masker changed the text before it was stored.
	Masked bool
	// Redacted describes the redaction that removed the text, empty while it is held.
	Redacted string
	// Random is the hex random value, which with the text and Event opens Commitment.
	Random string
	// Event is the decision event id the commitment is bound to.
	Event string
	// Commitment is the commitment the chain holds.
	Commitment string
	// Correction marks a correction, rendered indented under the decision it belongs to.
	Correction bool
	// SoD is the separation-of-duties line for a decision on an agent's run.
	SoD string
}

// identityView is an agent-initiated run's identity evidence as the dossier renders it.
type identityView struct {
	// InitiatedBy is the agent.
	InitiatedBy string
	// BoundTo is the account it acted under.
	BoundTo string
	// ProvisionedBy is who minted its token.
	ProvisionedBy string
	// ApprovedBy names each person who approved it or one of its steps.
	ApprovedBy string
}

// reasonRows renders a run's decision records as a thread: each decision followed by its
// corrections, oldest first.
func reasonRows(records []*decision.Record) []reasonRow {
	var decisions []*decision.Record
	corrections := map[string][]*decision.Record{}
	for _, rec := range records {
		if rec.Kind == decision.KindCorrection {
			corrections[rec.DecisionID] = append(corrections[rec.DecisionID], rec)
			continue
		}
		decisions = append(decisions, rec)
	}
	var rows []reasonRow
	for _, d := range decisions {
		rows = append(rows, reasonRowOf(d))
		for _, c := range corrections[d.ID] {
			rows = append(rows, reasonRowOf(c))
		}
	}
	return rows
}

// reasonRowOf renders one record.
func reasonRowOf(rec *decision.Record) reasonRow {
	row := reasonRow{Actor: rec.Actor, At: rec.At.UTC().Format(time.RFC3339), Event: rec.ID,
		Step: rec.StepRunID, Correction: rec.Kind == decision.KindCorrection}
	if rec.OnBehalfOf != "" && rec.OnBehalfOf != rec.Actor {
		row.Actor += " on behalf of " + rec.OnBehalfOf
	}
	switch {
	case row.Correction:
		row.Kind = "Correction"
	case rec.Verdict == "approved":
		row.Kind = "Approved"
	default:
		row.Kind = "Rejected"
	}
	if r := rec.Reason; r != nil {
		row.Commitment, row.Masked = r.Commitment, r.Masked
		if r.Redacted != nil {
			row.Redacted = fmt.Sprintf("redacted %s by %s (%s)",
				r.Redacted.At.UTC().Format(time.RFC3339), r.Redacted.Actor,
				strings.ReplaceAll(r.Redacted.Category, "_", " "))
		} else {
			row.Reason, row.Random = r.Text, r.Random
		}
	}
	if s := rec.SeparationOfDuties; s != nil {
		row.SoD = sodSentence(s)
	}
	return row
}

// sodSentence says how separation of duties applied to a decision on an agent's run.
func sodSentence(s *decision.SeparationOfDuties) string {
	required := "No rule required an independent approver"
	if s.Required {
		required = "A rule required an independent approver"
	}
	who := "the same account as"
	if s.Independent {
		who = "independent of"
	}
	return fmt.Sprintf("%s. The decider, %s, is %s the requester %s, the agent's bound account. "+
		"Result: %s.", required, s.Decider, who, s.Requester, strings.ReplaceAll(s.Result, "_", " "))
}

// agentIdentity renders an agent-initiated run's identity evidence, nil for any other run. The
// approvers are read from the decision records, the run's own and its steps'.
func agentIdentity(r *run.Run, records []*decision.Record) *identityView {
	if r == nil || r.Initiator == nil {
		return nil
	}
	i := r.Initiator
	view := &identityView{InitiatedBy: i.InitiatedBy, BoundTo: i.BoundTo,
		ProvisionedBy: i.ProvisionedBy}
	if view.BoundTo == "" {
		view.BoundTo = "no account recorded"
	}
	if view.ProvisionedBy == "" {
		view.ProvisionedBy = "not recorded: the agent's token predates issuer records"
	} else if i.ProvisionedByType != "" {
		view.ProvisionedBy += " (" + i.ProvisionedByType + ")"
	}
	var approvers []string
	for _, rec := range records {
		if rec.Kind == decision.KindDecision && rec.Verdict == "approved" {
			approvers = append(approvers, rec.Actor)
		}
	}
	view.ApprovedBy = strings.Join(approvers, ", ")
	if view.ApprovedBy == "" {
		view.ApprovedBy = "nobody: no person approved this run"
	}
	return view
}
