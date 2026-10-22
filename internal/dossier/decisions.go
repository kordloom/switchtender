package dossier

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
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
	// Comment says which pull request comment the decision was made from, empty for a decision
	// made any other way.
	Comment string
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

// deciderOf names who made a decision record from the chain entry that committed it, which the
// link binds and a seal can cover, rather than from the record, which nothing binds. The record's
// name is shown only as a disagreement: the entry's name and then the record's. The record is
// compared in the form the chain stores a name in, so a name the chain escaped does not read as a
// disagreement. A record with no entry among entries is named as the record names it. note is the
// seal check's note for the entry, appended when there is one.
func deciderOf(rec *decision.Record, entries map[string]*audit.Entry, note string) string {
	e, ok := entries[rec.ID]
	if !ok {
		name := rec.Actor
		if rec.OnBehalfOf != "" && rec.OnBehalfOf != rec.Actor {
			name += " on behalf of " + rec.OnBehalfOf
		}
		return withNote(name, note)
	}
	name := e.Actor
	if account := onBehalfOf(e); account != "" {
		name += " on behalf of " + account
	}
	if stored := audit.AsStored(&audit.Entry{Actor: rec.Actor}).Actor; stored != e.Actor {
		name += " (the decision record names " + rec.Actor + ")"
	}
	return withNote(name, note)
}

// entriesByID indexes a run's chain entries by id.
func entriesByID(entries []*audit.Entry) map[string]*audit.Entry {
	out := make(map[string]*audit.Entry, len(entries))
	for _, e := range entries {
		out[e.ID] = e
	}
	return out
}

// reasonRows renders a run's decision records as a thread: each decision followed by its
// corrections, oldest first. Each is named from the chain entry that committed it, with the note
// its seal check carries.
func reasonRows(records []*decision.Record, entries []*audit.Entry,
	notes map[string]string) []reasonRow {
	byID := entriesByID(entries)
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
		rows = append(rows, reasonRowOf(d, deciderOf(d, byID, notes[d.ID])))
		for _, c := range corrections[d.ID] {
			rows = append(rows, reasonRowOf(c, deciderOf(c, byID, notes[c.ID])))
		}
	}
	return rows
}

// reasonRowOf renders one record, made by decider.
func reasonRowOf(rec *decision.Record, decider string) reasonRow {
	row := reasonRow{Actor: decider, At: rec.At.UTC().Format(time.RFC3339), Event: rec.ID,
		Step: rec.StepRunID, Correction: rec.Kind == decision.KindCorrection}
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
	if c := rec.Comment; c != nil {
		row.Comment = commentSentence(c)
	}
	return row
}

// commentSentence says which pull request comment a decision was made from, by the forge's numeric
// ids and the SHA-256 of the body the webhook delivered, which an edit to the comment cannot
// change.
func commentSentence(c *decision.Comment) string {
	what := "pull request"
	if c.Forge == "gitlab" {
		what = "merge request"
	}
	return fmt.Sprintf("Decided from a %s comment: %s %s %s #%d, comment %d by %s account %d, "+
		"body SHA-256 %s.", what, c.Forge, c.APIURL, c.Repository, c.PullRequest, c.CommentID,
		c.Forge, c.AuthorID, c.BodySHA256)
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
// approvals are read from the decision records, the run's own and its steps', and each approver is
// named from the chain entry that committed the approval, with the note its seal check carries.
func agentIdentity(r *run.Run, records []*decision.Record, entries []*audit.Entry,
	notes map[string]string) *identityView {
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
	byID := entriesByID(entries)
	var approvers []string
	for _, rec := range records {
		if rec.Kind == decision.KindDecision && rec.Verdict == "approved" {
			if e, ok := byID[rec.ID]; ok {
				name := e.Actor
				if stored := audit.AsStored(&audit.Entry{Actor: rec.Actor}).Actor; stored != e.Actor {
					name += " (the decision record names " + rec.Actor + ")"
				}
				approvers = append(approvers, withNote(name, notes[rec.ID]))
				continue
			}
			approvers = append(approvers, withNote(rec.Actor, notes[rec.ID]))
		}
	}
	view.ApprovedBy = strings.Join(approvers, ", ")
	if view.ApprovedBy == "" {
		view.ApprovedBy = "nobody: no person approved this run"
	}
	return view
}
