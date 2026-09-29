package server

import (
	"errors"
	"net/http"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
)

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

// approveRunHandler releases a run held for approval so it can execute.
func approveRunHandler(approver Approver, store run.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if approver == nil {
			respondError(w, log, http.StatusNotFound, "approvals not enabled")
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
			if authorizeRunAccess(w, r, authz, log, rn) {
				return
			}
			// Separation of duties is enforced here as well as in the dispatcher, because only here is
			// the caller's account in hand. The actor recorded on a run is the credential's name, a
			// token's label or a username, so the dispatcher's comparison of names cannot tell that a
			// person submitting with their token and approving in their browser is one person.
			if denySelfApproval(w, r, log, rn) {
				return
			}
		}
		created, err := approver.Approve(r.Context(), r.PathValue("id"), actorName(r), actorType(r))
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
		case errors.Is(err, dispatch.ErrSelfApproval):
			// Separation of duties. The message carries the rule's own words rather than a bare
			// status, because the caller's next move is to find a second person.
			respondError(w, log, http.StatusConflict, err.Error())
			return
		case err != nil:
			log.Error("server: approve run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not approve run")
			return
		}
		respondJSON(w, log, http.StatusOK, maskRun(created), wantsPretty(r))
	}
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
			Reason string `json:"reason"`
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
			if authorizeRunAccess(w, r, authz, log, rn) {
				return
			}
		}
		created, err := approver.Reject(r.Context(), r.PathValue("id"), req.Reason, actorName(r), actorType(r))
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
		case err != nil:
			log.Error("server: reject run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not reject run")
			return
		}
		respondJSON(w, log, http.StatusOK, maskRun(created), wantsPretty(r))
	}
}
