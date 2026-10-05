package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// reasonHarness is a dispatcher with a chain and a held run routed to a queue nobody serves, so a
// released run stays pending and the test reads what the decision left behind.
type reasonHarness struct {
	// d is the dispatcher.
	d *Dispatcher
	// store holds the runs.
	store run.Store
	// audits is the chain.
	audits audit.Store
}

// newReasonHarness builds the harness.
func newReasonHarness(t *testing.T) *reasonHarness {
	t.Helper()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	d := New(store, okRunner(), zap.NewNop(), WithAudits(audits), WithNoJanitor())
	t.Cleanup(d.Close)
	return &reasonHarness{d: d, store: store, audits: audits}
}

// hold stores a held run, with the given changes applied, and returns it.
func (h *reasonHarness) hold(t *testing.T, change func(*run.Run)) *run.Run {
	t.Helper()
	r := &run.Run{
		ID: run.NewID(), Tool: run.ToolBash, Command: "deploy --env prod", Queue: "served-by-nobody",
		Status: run.StatusPendingApproval, Actor: "laptop", ActorType: "token",
		ActorUserID: "user_requester", CreatedAt: time.Now(), HeldByPolicy: "production changes",
		ExtraVars: map[string]any{"db_password": "correct-horse-battery"},
	}
	if change != nil {
		change(r)
	}
	if err := h.store.Save(context.Background(), r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return r
}

// decisions returns the chain's decision entries for a run.
func (h *reasonHarness) decisions(t *testing.T, runID string) []*audit.Entry {
	t.Helper()
	chain, err := h.audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []*audit.Entry
	for _, e := range chain {
		if e.Method == audit.MethodDecision && strings.Contains(e.Path, "/runs/"+runID+"/") {
			out = append(out, e)
		}
	}
	return out
}

// approver is a person deciding through their session.
var approver = outcome.Decider{Name: "ops-admin", Type: "session", OnBehalfOf: "ops-admin",
	AccountID: "user_approver"}

// TestAReasonIsMaskedCommittedAndOpenable proves the whole path a reason takes. It is masked before
// anything is recorded, a masked reason waits for the approver to confirm what will be kept, and
// the chain commits only a hiding commitment that the stored text and random value open.
//
// The masking cases are the control for the confirmation: an ordinary reason is recorded as written
// on the first call, so the refusal on the others is the masker having changed the text.
//
//nolint:funlen // Test function.
func TestAReasonIsMaskedCommittedAndOpenable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Reason is what the approver typed.
		Reason string
		// WantText is what is stored, committed, and disclosed.
		WantText string
		// WantMasked is whether the masker changed it, which needs a confirmation first.
		WantMasked bool
	}{{ // Test 0: An ordinary reason is recorded as written, in one call.
		Reason:   "  approved, change window confirmed with the database team\r\n",
		WantText: "approved, change window confirmed with the database team",
	}, { // Test 1: A secret-looking assignment is masked and confirmed before it is kept.
		Reason:     "approved, used token=ghp_abc123def456 to check",
		WantText:   "approved, used token=*** to check",
		WantMasked: true,
	}, { // Test 2: The run's own secret value is masked wherever it appears.
		Reason:     "approved after testing with correct-horse-battery",
		WantText:   "approved after testing with ***",
		WantMasked: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newReasonHarness(t)
			held := h.hold(t, nil)
			_, err := h.d.DecideRun(ctx, held.ID, RunDecision{Approve: true, Reason: test.Reason,
				By: approver})
			if test.WantMasked {
				var masked *ReasonMaskedError
				if !errors.As(err, &masked) {
					t.Fatalf("DecideRun() error = %v, want a confirmation of the masked text", err)
				}
				if diff := cmp.Diff(test.WantText, masked.Masked); diff != "" {
					t.Errorf("masked text mismatch (-want +got):\n%s", diff)
				}
				// Nothing is recorded until the approver confirms what will be kept.
				if n := len(h.decisions(t, held.ID)); n != 0 {
					t.Fatalf("an unconfirmed masked reason left %d decision entries", n)
				}
				if got, _ := h.store.Get(ctx, held.ID); got.Status != run.StatusPendingApproval {
					t.Fatalf("an unconfirmed masked reason released the run: %s", got.Status)
				}
				_, err = h.d.DecideRun(ctx, held.ID, RunDecision{Approve: true, Reason: test.Reason,
					ConfirmedMask: masked.Masked, By: approver})
			}
			if err != nil {
				t.Fatalf("DecideRun() error = %v", err)
			}
			entries := h.decisions(t, held.ID)
			if len(entries) != 1 {
				t.Fatalf("decision entries = %d, want 1", len(entries))
			}
			rec, err := h.d.DecisionStore().Get(ctx, entries[0].ID)
			if err != nil {
				t.Fatalf("the decision entry has no record under its id: %v", err)
			}
			if diff := cmp.Diff(test.WantText, rec.Reason.Text); diff != "" {
				t.Errorf("stored reason mismatch (-want +got):\n%s", diff)
			}
			if rec.Reason.Masked != test.WantMasked {
				t.Errorf("Masked = %v, want %v", rec.Reason.Masked, test.WantMasked)
			}
			if strings.Contains(rec.Reason.Text, "ghp_abc123def456") ||
				strings.Contains(rec.Reason.Text, "correct-horse-battery") {
				t.Errorf("the stored reason kept the secret: %q", rec.Reason.Text)
			}
			if !decision.Verify(rec.Reason.Commitment, rec.ID, rec.Reason.Random, rec.Reason.Text) {
				t.Error("the stored text and random value do not open the stored commitment")
			}
			got, err := h.store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			body, _, err := outcome.DecisionBodyWith(got, "approved", outcome.ExtrasOf(rec))
			if err != nil {
				t.Fatalf("DecisionBodyWith() error = %v", err)
			}
			if !audit.VerifyContentDigest(entries[0].ContentDigest, entries[0].Nonce, body) {
				t.Error("the decision entry does not commit the body carrying the reason commitment")
			}
			if !strings.Contains(string(body), rec.Reason.Commitment) ||
				strings.Contains(string(body), rec.Reason.Text) {
				t.Errorf("the committed body must carry the commitment and never the text: %s", body)
			}
		})
	}
}

// TestAReasonOverTheCapIsRefusedAndNothingRecorded pins the cap and that a refused decision leaves
// nothing on the chain and the run held.
func TestAReasonOverTheCapIsRefusedAndNothingRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newReasonHarness(t)
	held := h.hold(t, nil)
	_, err := h.d.DecideRun(ctx, held.ID, RunDecision{Approve: true,
		Reason: strings.Repeat("x", decision.MaxReasonChars+1), By: approver})
	if !errors.Is(err, ErrReasonTooLong) {
		t.Fatalf("DecideRun() error = %v, want ErrReasonTooLong", err)
	}
	if n := len(h.decisions(t, held.ID)); n != 0 {
		t.Errorf("a refused decision left %d decision entries", n)
	}
	if _, err := h.d.DecideRun(ctx, held.ID, RunDecision{Approve: true,
		Reason: strings.Repeat("x", decision.MaxReasonChars), By: approver}); err != nil {
		t.Errorf("a reason exactly at the cap was refused: %v", err)
	}
}

// TestARuleCanRequireAReason pins what a rule's requirement demands of each decision, and that a
// refused decision records nothing.
func TestARuleCanRequireAReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Requirement is what the rule that held the run copied onto it.
		Requirement string
		// Approve is the decision.
		Approve bool
		// Reason is what the decider gave.
		Reason string
		// Want is the error.
		Want error
	}{{ // Test 0: No requirement, no reason, an approval goes through.
		Requirement: "", Approve: true, Want: nil,
	}, { // Test 1: A denial-only requirement leaves an approval free.
		Requirement: decision.RequireDenials, Approve: true, Want: nil,
	}, { // Test 2: A denial-only requirement refuses a bare denial.
		Requirement: decision.RequireDenials, Approve: false, Want: ErrReasonRequired,
	}, { // Test 3: The same denial with a reason goes through.
		Requirement: decision.RequireDenials, Approve: false, Reason: "not during the freeze",
		Want: nil,
	}, { // Test 4: An always requirement refuses a bare approval.
		Requirement: decision.RequireAlways, Approve: true, Want: ErrReasonRequired,
	}, { // Test 5: Whitespace is not a reason.
		Requirement: decision.RequireAlways, Approve: true, Reason: " \n ", Want: ErrReasonRequired,
	}, { // Test 6: An always requirement is met by a reason.
		Requirement: decision.RequireAlways, Approve: true, Reason: "ticket OPS-12 approved",
		Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newReasonHarness(t)
			held := h.hold(t, func(r *run.Run) { r.RequireReason = test.Requirement })
			_, err := h.d.DecideRun(ctx, held.ID, RunDecision{Approve: test.Approve,
				Reason: test.Reason, By: approver})
			if !errors.Is(err, test.Want) {
				t.Fatalf("DecideRun() error = %v, want %v", err, test.Want)
			}
			wantEntries := 1
			if test.Want != nil {
				wantEntries = 0
				if !strings.Contains(err.Error(), "production changes") {
					t.Errorf("the refusal %q does not name the rule that asked", err)
				}
			}
			if n := len(h.decisions(t, held.ID)); n != wantEntries {
				t.Errorf("decision entries = %d, want %d", n, wantEntries)
			}
		})
	}
}

// TestAnAgentsRunRecordsSeparationOfDuties pins the evaluation a decision on an agent's run
// carries. The account the agent is bound to may approve its runs unless a rule requires an
// independent approver, and it counts as the requester when one does.
func TestAnAgentsRunRecordsSeparationOfDuties(t *testing.T) {
	t.Parallel()
	bound := outcome.Decider{Name: "dev-lead", Type: "session", OnBehalfOf: "dev-lead",
		AccountID: "user_dev_lead"}
	tests := []struct {
		// Required is whether the rule that held the run requires an independent approver.
		Required bool
		// By decides.
		By outcome.Decider
		// Approve is the decision.
		Approve bool
		// WantSoD is the evaluation recorded, nil when the decision is refused.
		WantSoD *decision.SeparationOfDuties
		// Want is the error.
		Want error
	}{{ // Test 0: The bound account approves where no rule requires otherwise.
		Required: false, By: bound, Approve: true,
		WantSoD: &decision.SeparationOfDuties{Requester: "dev-lead", Decider: "dev-lead",
			Result: decision.SoDNotRequired},
	}, { // Test 1: With separation of duties on, the bound account counts as the requester.
		Required: true, By: bound, Approve: true, Want: ErrSelfApproval,
	}, { // Test 2: An independent person approves and the requirement is satisfied.
		Required: true, By: approver, Approve: true,
		WantSoD: &decision.SeparationOfDuties{Required: true, Requester: "dev-lead",
			Decider: "ops-admin", Independent: true, Result: decision.SoDSatisfied},
	}, { // Test 3: The bound account may still deny, which separation of duties never restricts.
		Required: true, By: bound, Approve: false,
		WantSoD: &decision.SeparationOfDuties{Required: true, Requester: "dev-lead",
			Decider: "dev-lead", Result: decision.SoDNotApplicable},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newReasonHarness(t)
			held := h.hold(t, func(r *run.Run) {
				r.Actor, r.ActorType, r.ActorUserID = "deploy-bot", "agent", "user_dev_lead"
				r.RequireDistinctApprover = test.Required
				r.Initiator = &run.Initiator{InitiatedBy: "deploy-bot", CredentialID: "tok_bot",
					BoundTo: "dev-lead", ProvisionedBy: "org-admin", ProvisionedByType: "session"}
			})
			_, err := h.d.DecideRun(ctx, held.ID, RunDecision{Approve: test.Approve, By: test.By})
			if !errors.Is(err, test.Want) {
				t.Fatalf("DecideRun() error = %v, want %v", err, test.Want)
			}
			entries := h.decisions(t, held.ID)
			if test.Want != nil {
				if len(entries) != 0 {
					t.Errorf("a refused decision left %d decision entries", len(entries))
				}
				return
			}
			if len(entries) != 1 {
				t.Fatalf("decision entries = %d, want 1", len(entries))
			}
			rec, err := h.d.DecisionStore().Get(ctx, entries[0].ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(test.WantSoD, rec.SeparationOfDuties); diff != "" {
				t.Errorf("separation of duties mismatch (-want +got):\n%s", diff)
			}
			got, err := h.store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			body, _, err := outcome.DecisionBodyWith(got, rec.Verdict, outcome.ExtrasOf(rec))
			if err != nil {
				t.Fatalf("DecisionBodyWith() error = %v", err)
			}
			if !audit.VerifyContentDigest(entries[0].ContentDigest, entries[0].Nonce, body) {
				t.Error("the decision entry does not commit the separation-of-duties evaluation")
			}
		})
	}
}

// TestCorrectionsAppendAndNeverEdit pins that a reason is never edited: a correction is a record
// and a chain entry of its own, naming the decision it corrects, and the original stays as it was.
//
//nolint:funlen // Test function.
func TestCorrectionsAppendAndNeverEdit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// DecisionID is the decision named, empty for the run's own decision.
		DecisionID string
		// Correction is what is appended.
		Correction Correction
		// WantCorrections is how many correction entries the chain holds afterward.
		WantCorrections int
		// Want is the error.
		Want error
	}{{ // Test 0: A correction is appended.
		Correction:      Correction{Text: "the ticket was OPS-12, not OPS-11", By: approver},
		WantCorrections: 1,
	}, { // Test 1: An unknown decision is refused.
		DecisionID: "aud_unknown", Correction: Correction{Text: "x", By: approver},
		Want: ErrDecisionNotFound,
	}, { // Test 2: An empty correction is refused.
		Correction: Correction{Text: "  ", By: approver}, Want: ErrNoReasonText,
	}, { // Test 3: An agent never writes one.
		Correction: Correction{Text: "fine", By: outcome.Decider{Name: "deploy-bot", Type: "agent"}},
		Want:       ErrAgentApproval,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newReasonHarness(t)
			held := h.hold(t, nil)
			if _, err := h.d.DecideRun(ctx, held.ID, RunDecision{Approve: true,
				Reason: "approved per ticket OPS-11", By: approver}); err != nil {
				t.Fatalf("DecideRun() error = %v", err)
			}
			original := h.decisions(t, held.ID)[0]
			before, err := h.d.DecisionStore().Get(ctx, original.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			named := test.DecisionID
			if named == "" {
				named = original.ID
			}
			rec, err := h.d.AddCorrection(ctx, held.ID, named, test.Correction)
			if !errors.Is(err, test.Want) {
				t.Fatalf("AddCorrection() error = %v, want %v", err, test.Want)
			}
			if test.Want == nil {
				if rec.Kind != decision.KindCorrection || rec.DecisionID != original.ID {
					t.Errorf("correction = %+v, want a correction of %s", rec, original.ID)
				}
				if !decision.Verify(rec.Reason.Commitment, rec.ID, rec.Reason.Random,
					rec.Reason.Text) {
					t.Error("the correction's text and random value do not open its commitment")
				}
			}
			after, err := h.d.DecisionStore().Get(ctx, original.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(before, after); diff != "" {
				t.Errorf("a correction edited the original (-want +got):\n%s", diff)
			}
			chain, err := h.audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			corrections := 0
			for _, e := range chain {
				if got, ok := decision.ParseReasonPath(e.Path); ok && e.Method == audit.MethodReason &&
					!got.Redacted && got.DecisionID == original.ID {
					corrections++
				}
			}
			if diff := cmp.Diff(test.WantCorrections, corrections); diff != "" {
				t.Errorf("correction entries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRedactionRemovesTextAndRandomAndKeepsTheCommitment pins redaction as a privacy action: the
// text and random value go together, the commitment stays, the chain records who, when, and why,
// and a second redaction or a bad category is refused.
func TestRedactionRemovesTextAndRandomAndKeepsTheCommitment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newReasonHarness(t)
	held := h.hold(t, nil)
	if _, err := h.d.DecideRun(ctx, held.ID, RunDecision{Approve: true,
		Reason: "approved after a call with the on-call engineer", By: approver}); err != nil {
		t.Fatalf("DecideRun() error = %v", err)
	}
	original := h.decisions(t, held.ID)[0]
	before, err := h.d.DecisionStore().Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	privacy := outcome.Decider{Name: "privacy-admin", Type: "session", OnBehalfOf: "privacy-admin"}
	if _, err := h.d.RedactReason(ctx, held.ID, original.ID, "nonsense", privacy); !errors.Is(err,
		ErrRedactionCategory) {
		t.Errorf("RedactReason(bad category) error = %v, want ErrRedactionCategory", err)
	}
	got, err := h.d.RedactReason(ctx, held.ID, original.ID, decision.CategoryPersonalData, privacy)
	if err != nil {
		t.Fatalf("RedactReason() error = %v", err)
	}
	if got.Reason.Text != "" || got.Reason.Random != "" {
		t.Errorf("redacted reason still holds text %q or random %q", got.Reason.Text,
			got.Reason.Random)
	}
	if got.Reason.Commitment != before.Reason.Commitment {
		t.Errorf("redaction changed the commitment from %s to %s", before.Reason.Commitment,
			got.Reason.Commitment)
	}
	if got.Reason.Redacted == nil || got.Reason.Redacted.Category != decision.CategoryPersonalData ||
		got.Reason.Redacted.Actor != "privacy-admin" || got.Reason.Redacted.EntryID == "" {
		t.Errorf("redaction record = %+v, want who, why, and the chain entry", got.Reason.Redacted)
	}
	chain, err := h.audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var entry *audit.Entry
	for _, e := range chain {
		if e.ID == got.Reason.Redacted.EntryID {
			entry = e
		}
	}
	if entry == nil || entry.Method != audit.MethodReason || entry.Actor != "privacy-admin" ||
		entry.Path != decision.RedactionPath(held.ID, original.ID, "", decision.CategoryPersonalData) {
		t.Errorf("redaction entry = %+v, want a REASON entry naming the decision and category", entry)
	}
	if _, err := h.d.RedactReason(ctx, held.ID, original.ID, decision.CategoryOther,
		privacy); !errors.Is(err, decision.ErrRedacted) {
		t.Errorf("second RedactReason() error = %v, want ErrRedacted", err)
	}
	if _, err := h.d.RedactReason(ctx, "run_elsewhere", original.ID, decision.CategoryOther,
		privacy); !errors.Is(err, ErrDecisionNotFound) {
		t.Errorf("RedactReason(other run) error = %v, want ErrDecisionNotFound", err)
	}
}

// failingAppends is an audit store whose appends of one method fail, so a decision whose chain
// entry cannot be written can be driven.
type failingAppends struct {
	audit.Store
	// method is the entry method whose appends fail.
	method string
}

// Append refuses entries of the failing method and passes every other through.
func (f *failingAppends) Append(ctx context.Context, e *audit.Entry) error {
	if e.Method == f.method {
		return errors.New("the audit store is unavailable")
	}
	return f.Store.Append(ctx, e)
}

// TestADecisionTheChainRefusesLeavesNoRecord pins that a decision record never stands for a
// decision the chain does not hold: the record is kept before the entry, and withdrawn when the
// entry fails.
func TestADecisionTheChainRefusesLeavesNoRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := &failingAppends{Store: audit.NewMemStore(), method: audit.MethodDecision}
	d := New(store, okRunner(), zap.NewNop(), WithAudits(audits), WithNoJanitor())
	t.Cleanup(d.Close)
	held := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy", Queue: "served-by-nobody",
		Status: run.StatusPendingApproval, CreatedAt: time.Now()}
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := d.DecideRun(ctx, held.ID, RunDecision{Approve: true, Reason: "fine",
		By: approver}); err == nil {
		t.Fatal("DecideRun() succeeded with the chain refusing the decision entry")
	}
	records, err := d.Decisions(ctx, held.ID)
	if err != nil {
		t.Fatalf("Decisions() error = %v", err)
	}
	if len(records) != 0 {
		t.Errorf("a decision the chain refused left %d records", len(records))
	}
	got, err := store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("status = %s, want the run still held", got.Status)
	}
}
