package server

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// ReasonedApprover decides a held run with the approver's reason. The dispatcher satisfies it. An
// approver that does not can still release and reject runs, but refuses a decision that carries a
// reason rather than dropping the reason.
type ReasonedApprover interface {
	// DecideRun approves or rejects the held run with id.
	DecideRun(ctx context.Context, id string, dec dispatch.RunDecision) (*run.Run, error)
}

// ReasonKeeper appends corrections to a decision's reason and redacts reasons. The dispatcher
// satisfies it.
type ReasonKeeper interface {
	// AddCorrection appends a correction to the decision with decisionID on the run with runID.
	AddCorrection(ctx context.Context, runID, decisionID string,
		c dispatch.Correction) (*decision.Record, error)
	// RedactReason removes the text and random value of the reason on the record with recordID.
	RedactReason(ctx context.Context, runID, recordID, category string,
		by outcome.Decider) (*decision.Record, error)
}

// maskedReasonResponse is the answer to a decision whose reason the secret masker changed: nothing
// was recorded, and the approver is shown the masked text to confirm.
type maskedReasonResponse struct {
	// Error says what happened and what to do.
	Error string `json:"error"`
	// MaskedReason is the reason as it would be stored. Sending it back as masked_reason with the same
	// reason records the decision with exactly this text.
	MaskedReason string `json:"masked_reason"`
}

// decisionsResponse is the GET /runs/{id}/decisions envelope.
type decisionsResponse struct {
	// Decisions are the run's decision records and their corrections, in thread order.
	Decisions []*decision.Record `json:"decisions"`
	// Count is how many there are.
	Count int `json:"count"`
}

// correctionRequest is the body of a correction.
type correctionRequest struct {
	// Text is the correction, masked before it is recorded.
	Text string `json:"text"`
	// MaskedText is the masked text the writer was shown and confirmed, for a correction the masker
	// changed.
	MaskedText string `json:"masked_text,omitempty"`
}

// redactRequest is the body of a reason redaction.
type redactRequest struct {
	// Category says why the reason is removed: personal_data, secret, or other.
	Category string `json:"category"`
}

// respondDecisionError writes the answer to a decision, correction, or redaction the dispatcher
// refused, and reports whether there was one to write.
func respondDecisionError(w http.ResponseWriter, log *zap.Logger, err error, op string) bool {
	if err == nil {
		return false
	}
	var masked *dispatch.ReasonMaskedError
	switch {
	case errors.As(err, &masked):
		respondJSON(w, log, http.StatusConflict, maskedReasonResponse{Error: err.Error(),
			MaskedReason: masked.Masked}, false)
	case errors.Is(err, dispatch.ErrAgentApproval):
		respondError(w, log, http.StatusForbidden, err.Error())
	case errors.Is(err, run.ErrNotFound), errors.Is(err, dispatch.ErrDecisionNotFound):
		respondError(w, log, http.StatusNotFound, err.Error())
	case errors.Is(err, dispatch.ErrReasonTooLong), errors.Is(err, dispatch.ErrNoReasonText),
		errors.Is(err, dispatch.ErrRedactionCategory):
		respondError(w, log, http.StatusBadRequest, err.Error())
	case errors.Is(err, dispatch.ErrReasonRequired), errors.Is(err, decision.ErrNoReason),
		errors.Is(err, decision.ErrRedacted):
		respondError(w, log, http.StatusConflict, err.Error())
	default:
		log.Error("server: " + op + ": " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not "+op)
	}
	return true
}

// readDecisionRun reads the run a decision route names and checks the caller may act on it,
// writing the refusal and returning nil when they may not.
func readDecisionRun(w http.ResponseWriter, r *http.Request, store run.Store, authz *authorizer,
	log *zap.Logger) *run.Run {
	got, err := store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, run.ErrNotFound) {
		respondError(w, log, http.StatusNotFound, "run not found")
		return nil
	}
	if err != nil {
		log.Error("server: read run: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read run")
		return nil
	}
	if authorizeRunAccess(w, r, authz, log, got) {
		return nil
	}
	return got
}

// runDecisionsHandler lists a run's decision records: each approval and denial a person made on it
// or on its workflow approval steps, the reason given with it, its corrections, a redaction where
// one happened, and for an agent's run how separation of duties was evaluated. Reasons are audit
// evidence, so reading them follows the evidence rule: an admin, or the actor who asked for the
// run, which is how an agent learns why its change was refused.
func runDecisionsHandler(store run.Store, decisions decision.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runDecisionsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if decisions == nil {
			respondError(w, log, http.StatusNotFound, "decision records not enabled")
			return
		}
		got := readDecisionRun(w, r, store, authz, log)
		if got == nil {
			return
		}
		if denyUnlessAdminOrActor(w, r, log, got) {
			return
		}
		records, err := decisions.ForRun(r.Context(), got.ID)
		if err == nil {
			// Only the decisions that took effect: the one the run, or each approval step, stores
			// as its winner. A record a losing decider left behind is never listed.
			records, err = outcome.EffectiveDecisions(r.Context(), store, records)
		}
		if err != nil {
			log.Error("server: list decision records: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list the decisions")
			return
		}
		if records == nil {
			records = []*decision.Record{}
		}
		respondJSON(w, log, http.StatusOK, decisionsResponse{Decisions: records,
			Count: len(records)}, wantsPretty(r))
	}
}

// addCorrectionHandler appends a correction to a decision's reason. A reason is never edited: the
// correction is recorded and committed on its own, naming the decision it corrects.
func addCorrectionHandler(store run.Store, approver Approver, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keeper, ok := approver.(ReasonKeeper)
		if !ok || store == nil {
			respondError(w, log, http.StatusNotFound, "decision records not enabled")
			return
		}
		var req correctionRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		got := readDecisionRun(w, r, store, authz, log)
		if got == nil {
			return
		}
		rec, err := keeper.AddCorrection(r.Context(), got.ID, r.PathValue("decision"),
			dispatch.Correction{Text: req.Text, ConfirmedMask: req.MaskedText, By: deciderOf(r)})
		if respondDecisionError(w, log, err, "record the correction") {
			return
		}
		respondJSON(w, log, http.StatusCreated, rec, wantsPretty(r))
	}
}

// redactReasonHandler removes a reason's text and random value, an explicit privacy action recorded
// on the chain with who, when, and why. The commitment stays and can never be opened again.
func redactReasonHandler(store run.Store, approver Approver, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keeper, ok := approver.(ReasonKeeper)
		if !ok || store == nil {
			respondError(w, log, http.StatusNotFound, "decision records not enabled")
			return
		}
		var req redactRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		got := readDecisionRun(w, r, store, authz, log)
		if got == nil {
			return
		}
		rec, err := keeper.RedactReason(r.Context(), got.ID, r.PathValue("record"), req.Category,
			deciderOf(r))
		if respondDecisionError(w, log, err, "redact the reason") {
			return
		}
		respondJSON(w, log, http.StatusOK, rec, wantsPretty(r))
	}
}
