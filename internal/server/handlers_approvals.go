package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// deciderOf names the caller making a decision exactly as the gate recorded the request carrying
// it: the name, how the caller authenticated, and the account whose authority it used. The decision
// entry and the request entry for one decision then agree. The decision entry carried the name and
// type alone, so the account behind a decision was recoverable only by pairing it with its request,
// and an install serving open recorded the request under a caller class and the decision under
// nobody. A handler reached with no gate in front has nobody to name.
func deciderOf(r *http.Request) outcome.Decider {
	who, _ := recordedFrom(r.Context())
	return outcome.Decider{Name: who.Name, Type: who.Type, OnBehalfOf: who.OnBehalfOf,
		AccountID: actorAccount(r)}
}

// denySelfApproval refuses an approval by the person who asked for the run, when the rule that held it
// requires a different approver. It reports whether the handler should stop.
//
// Rejecting your own run is untouched: withdrawing a request needs nobody else, and blocking it would
// leave a requester unable to take back their own change.
func denySelfApproval(w http.ResponseWriter, r *http.Request, log *zap.Logger, rn *run.Run) bool {
	if rn == nil || !rn.RequireDistinctApprover {
		return false
	}
	actor, ok := actorFrom(r.Context())
	if !ok || !sameActor(actor, rn) {
		return false
	}
	respondError(w, log, http.StatusConflict, "the rule that held this run requires a different "+
		"person to approve it, and you are the one who asked for it. You can still reject it to "+
		"withdraw the request")
	return true
}

// approveRequest is the optional body of an approve or reject call.
type approveRequest struct {
	// StateDigest is the state digest the approver was shown for a workflow approval step. When it is
	// set the decision is refused unless the workflow still reduces to it, so the approval binds to
	// what was looked at. It applies only to an approval step.
	StateDigest string `json:"state_digest,omitempty"`
	// Reason is the approver's optional stated reason, up to 1,000 characters. It is masked for known
	// secrets before anything records it, kept as audit evidence beside the decision, and committed
	// to the chain as a hiding commitment, never as text.
	Reason string `json:"reason,omitempty"`
	// MaskedReason confirms the masked form of Reason the approver was shown. When the masker
	// changes a reason, the decision is refused with the masked text, recording nothing, until it is
	// sent again with this set to exactly that text.
	MaskedReason string `json:"masked_reason,omitempty"`
}

// approveRunHandler releases a run held for approval so it can execute, or approves a workflow
// approval step, posted to the step's own id, so the workflow continues down its approve path.
func approveRunHandler(approver Approver, store run.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if approver == nil {
			respondError(w, log, http.StatusNotFound, "approvals not enabled")
			return
		}
		var req approveRequest
		if !decodeStrictOptional(w, log, r.Body, &req) {
			return
		}
		// A decision on a run is a decision about the objects it will touch, so the approver has to
		// be someone who may use them. Every other run mutation checks; these two did not, and they
		// are the two that release a held run onto real hosts.
		if store != nil {
			rn, gerr := store.Get(r.Context(), r.PathValue("id"))
			if errors.Is(gerr, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			if gerr != nil {
				log.Error("server: read run: " + gerr.Error())
				respondError(w, log, http.StatusInternalServerError, "could not read run")
				return
			}
			target, step, ok := resolveDecisionTarget(w, r, store, authz, log, rn)
			if !ok {
				return
			}
			if authorizeRunAccess(w, r, authz, log, target) {
				return
			}
			// Separation of duties is enforced here as well as in the dispatcher, because only here is
			// the caller's account in hand. The actor recorded on a run is the credential's name, a
			// token's label or a username, so the dispatcher's comparison of names cannot tell that a
			// person submitting with their token and approving in their browser is one person.
			if denySelfApproval(w, r, log, target) {
				return
			}
			if step {
				decideStep(w, r, approver, log, target, dispatch.StepDecision{
					Approve: true, Reason: req.Reason, ConfirmedMask: req.MaskedReason,
					Shown: req.StateDigest, By: deciderOf(r),
				})
				return
			}
		}
		if req.StateDigest != "" {
			respondError(w, log, http.StatusBadRequest,
				"state_digest applies to a workflow approval step, and this is a run")
			return
		}
		created, err := decideRun(r.Context(), approver, r.PathValue("id"), dispatch.RunDecision{
			Approve: true, Reason: req.Reason, ConfirmedMask: req.MaskedReason, By: deciderOf(r),
		})
		if reasonRefusal(w, log, err) {
			return
		}
		switch {
		case errors.Is(err, run.ErrNotFound):
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		case errors.Is(err, dispatch.ErrNotPendingApproval):
			respondError(w, log, http.StatusConflict, "run is not awaiting approval")
			return
		case errors.Is(err, dispatch.ErrChildNotApprovable):
			respondError(w, log, http.StatusConflict,
				"a shard or step is decided through its parent, not on its own")
			return
		case errors.Is(err, dispatch.ErrStepPending):
			respondError(w, log, http.StatusConflict, err.Error())
			return
		case errors.Is(err, dispatch.ErrSelfApproval):
			// Separation of duties. The message carries the rule's own words rather than a bare
			// status, because the caller's next move is to find a second person.
			respondError(w, log, http.StatusConflict, err.Error())
			return
		case errors.Is(err, dispatch.ErrAgentApproval):
			// The dispatcher's own lock, reached only when an agent's decision got past the door.
			respondError(w, log, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Error("server: approve run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not approve run")
			return
		}
		respondRun(w, r, log, http.StatusOK, created)
	}
}

// decideRun applies one decision on a held run through the approver. An approver that records
// reasons takes the whole decision. One that does not is asked to approve or reject, and a decision
// carrying a reason it could not record is refused rather than recorded without it, since a reason
// the approver was told is kept as audit evidence must not quietly disappear.
func decideRun(ctx context.Context, approver Approver, id string,
	dec dispatch.RunDecision) (*run.Run, error) {
	if reasoned, ok := approver.(ReasonedApprover); ok {
		return reasoned.DecideRun(ctx, id, dec)
	}
	if dec.Reason != "" {
		return nil, errReasonUnrecorded
	}
	if dec.Approve {
		return approver.Approve(ctx, id, dec.By)
	}
	return approver.Reject(ctx, id, "", dec.By)
}

// errReasonUnrecorded is returned when a decision carries a reason the approver behind the route
// cannot record.
var errReasonUnrecorded = errors.New("this server cannot record a decision's reason, so a " +
	"decision carrying one is refused rather than recorded without it")

// reasonRefusal writes the answer to a decision refused over its reason: one the masker changed
// that the approver has not confirmed, one a rule required and was not given, one over the cap, or
// one the approver cannot record. It reports whether the error was one of these.
func reasonRefusal(w http.ResponseWriter, log *zap.Logger, err error) bool {
	var masked *dispatch.ReasonMaskedError
	switch {
	case err == nil:
		return false
	case errors.As(err, &masked), errors.Is(err, dispatch.ErrReasonRequired),
		errors.Is(err, dispatch.ErrReasonTooLong):
		return respondDecisionError(w, log, err, "decide")
	case errors.Is(err, errReasonUnrecorded):
		respondError(w, log, http.StatusNotImplemented, err.Error())
		return true
	}
	return false
}

// StepApprover decides workflow approval steps. The dispatcher satisfies it, and an approver that
// does not leaves approval steps undecidable through the API rather than decided some other way.
type StepApprover interface {
	// DecideStep approves or denies the approval step with id.
	DecideStep(ctx context.Context, id string, dec dispatch.StepDecision) (*run.Run, error)
}

// resolveDecisionTarget returns what a decision posted to rn decides: rn itself, or, for rn an
// approval step, that step. A decision posted to a workflow that is waiting at an approval step is
// refused with 409, naming each waiting step and the call that decides it, once the caller has
// shown it may use the workflow. Only the step's own decision may move the workflow: the step
// carries the rules that apply to it alone, and the state its approver is shown binds to the step
// rather than to the workflow. A release that predates approval steps refuses the same call,
// because it reads the stored status of a parked workflow as one it does not know, so the same
// request means the same thing on either side of an upgrade or a rollback. It writes the response
// and reports false when the decision cannot go ahead.
func resolveDecisionTarget(w http.ResponseWriter, r *http.Request, store run.Store, authz *authorizer,
	log *zap.Logger, rn *run.Run) (*run.Run, bool, bool) {
	if rn.Kind == run.KindApproval {
		return rn, true, true
	}
	if rn.Kind != run.KindPipeline || rn.Status.Terminal() {
		return rn, false, true
	}
	pending, err := run.PendingApprovalSteps(r.Context(), store, rn.ID)
	if err != nil {
		log.Error("server: list waiting approval steps: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read the workflow's steps")
		return nil, false, false
	}
	if len(pending) == 0 {
		return rn, false, true
	}
	if authorizeRunAccess(w, r, authz, log, rn) {
		return nil, false, false
	}
	respondError(w, log, http.StatusConflict, waitingStepsRefusal(pending))
	return nil, false, false
}

// waitingStepsRefusal says why a decision posted to a workflow waiting at approval steps is
// refused, naming each step and the call that decides it.
func waitingStepsRefusal(pending []*run.Run) string {
	calls := make([]string, 0, len(pending))
	for _, p := range pending {
		calls = append(calls, fmt.Sprintf("approval step %q is decided with POST /v1/runs/%s/approve "+
			"or POST /v1/runs/%s/reject", p.StepName, p.ID, p.ID))
	}
	what := "an approval step"
	if len(pending) > 1 {
		what = strconv.Itoa(len(pending)) + " approval steps"
	}
	return "this workflow is waiting at " + what + ", and only a step's own decision moves it: " +
		strings.Join(calls, ", and ")
}

// decideStep applies one decision to a workflow approval step and writes the response.
func decideStep(w http.ResponseWriter, r *http.Request, approver Approver, log *zap.Logger,
	step *run.Run, dec dispatch.StepDecision) {
	stepper, ok := approver.(StepApprover)
	if !ok {
		respondError(w, log, http.StatusNotFound, "workflow approval steps not enabled")
		return
	}
	// An agent's token is capped below the role this route needs, so this is the second line, kept
	// for any path that reaches here with an agent and an approve in hand.
	if actor, found := actorFrom(r.Context()); found && actor.Agent && dec.Approve {
		respondError(w, log, http.StatusForbidden, "an agent cannot approve a workflow approval step")
		return
	}
	decided, err := stepper.DecideStep(r.Context(), step.ID, dec)
	if reasonRefusal(w, log, err) {
		return
	}
	switch {
	case errors.Is(err, run.ErrNotFound):
		respondError(w, log, http.StatusNotFound, "run not found")
		return
	case errors.Is(err, dispatch.ErrAgentApproval):
		respondError(w, log, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, dispatch.ErrNotPendingApproval), errors.Is(err, dispatch.ErrSelfApproval),
		errors.Is(err, dispatch.ErrStateMoved), errors.Is(err, dispatch.ErrNotApprovalStep):
		respondError(w, log, http.StatusConflict, err.Error())
		return
	case err != nil:
		log.Error("server: decide approval step: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not decide the approval step")
		return
	}
	respondRun(w, r, log, http.StatusOK, decided)
}

// rejectRunHandler denies a run held for approval, recording an optional reason as its error.
func rejectRunHandler(approver Approver, store run.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if approver == nil {
			respondError(w, log, http.StatusNotFound, "approvals not enabled")
			return
		}
		var req struct {
			// Reason is the decider's optional stated reason, masked and committed as an approval's
			// is, and kept in the decision record rather than in the run's error.
			Reason string `json:"reason"`
			// MaskedReason confirms the masked form of Reason the decider was shown.
			MaskedReason string `json:"masked_reason,omitempty"`
			// StateDigest is the state digest the decider was shown for a workflow approval step.
			StateDigest string `json:"state_digest,omitempty"`
		}
		// A rejection needs no reason, so an absent body is fine, but a body that is present is held
		// to the same rule as every other: a misspelled reason is refused rather than dropped, so the
		// audit trail never records a rejection whose stated cause quietly went missing.
		if !decodeStrictOptional(w, log, r.Body, &req) {
			return
		}
		// A decision on a run is a decision about the objects it will touch, so the approver has to
		// be someone who may use them. Every other run mutation checks; these two did not, and they
		// are the two that release a held run onto real hosts.
		if store != nil {
			rn, gerr := store.Get(r.Context(), r.PathValue("id"))
			if errors.Is(gerr, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			if gerr != nil {
				log.Error("server: read run: " + gerr.Error())
				respondError(w, log, http.StatusInternalServerError, "could not read run")
				return
			}
			target, step, ok := resolveDecisionTarget(w, r, store, authz, log, rn)
			if !ok {
				return
			}
			if authorizeRunAccess(w, r, authz, log, target) {
				return
			}
			if step {
				decideStep(w, r, approver, log, target, dispatch.StepDecision{
					Reason: req.Reason, ConfirmedMask: req.MaskedReason, Shown: req.StateDigest,
					By: deciderOf(r),
				})
				return
			}
		}
		if req.StateDigest != "" {
			respondError(w, log, http.StatusBadRequest,
				"state_digest applies to a workflow approval step, and this is a run")
			return
		}
		created, err := decideRun(r.Context(), approver, r.PathValue("id"), dispatch.RunDecision{
			Reason: req.Reason, ConfirmedMask: req.MaskedReason, By: deciderOf(r),
		})
		if reasonRefusal(w, log, err) {
			return
		}
		switch {
		case errors.Is(err, run.ErrNotFound):
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		case errors.Is(err, dispatch.ErrNotPendingApproval):
			respondError(w, log, http.StatusConflict, "run is not awaiting approval")
			return
		case errors.Is(err, dispatch.ErrChildNotApprovable):
			respondError(w, log, http.StatusConflict,
				"a shard or step is decided through its parent, not on its own")
			return
		case errors.Is(err, dispatch.ErrStepPending):
			respondError(w, log, http.StatusConflict, err.Error())
			return
		case err != nil:
			log.Error("server: reject run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not reject run")
			return
		}
		respondRun(w, r, log, http.StatusOK, created)
	}
}
