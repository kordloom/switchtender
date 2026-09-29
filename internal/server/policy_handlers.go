package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// createPolicyRequest is the JSON body accepted by POST /policies.
type createPolicyRequest struct {
	// Name labels the policy. Required.
	Name string `json:"name"`
	// Tool matches a run's execution tool, empty for any.
	Tool string `json:"tool,omitempty"`
	// CommandContains matches when a run's command contains this text, empty for any.
	CommandContains string `json:"command_contains,omitempty"`
	// InventoryID matches a run targeting this stored inventory, empty for any.
	InventoryID string `json:"inventory_id,omitempty"`
	// ExcludeDryRun leaves dry-run runs unmatched.
	ExcludeDryRun bool `json:"exclude_dry_run,omitempty"`
	// MaxDestroy holds a matched terraform or opentofu run for approval when its plan would destroy
	// more than this many resources. A pointer so a missing field disables the plan-content check
	// rather than holding on any destroy, and a negative value disables it explicitly.
	MaxDestroy *int `json:"max_destroy,omitempty"`
}

// resolveMaxDestroy returns the request's max_destroy, defaulting a missing value to the disabled
// sentinel so a policy created without the field leaves the plan-content check off rather than
// holding on any destroy.
func resolveMaxDestroy(v *int) int {
	if v == nil {
		return policy.DisabledMaxDestroy
	}
	return *v
}

// listPoliciesResponse wraps the policy list.
type listPoliciesResponse struct {
	// Policies is the list of approval policies.
	Policies []*policy.Policy `json:"policies"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createPolicyHandler stores a new approval policy.
func createPolicyHandler(store policy.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "policies not enabled")
			return
		}
		var req createPolicyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		if req.Tool != "" && !run.ValidTool(req.Tool) {
			respondError(w, log, http.StatusBadRequest, "tool is not a supported execution tool")
			return
		}
		p := &policy.Policy{
			ID: policy.NewID(), Name: req.Name, Tool: req.Tool,
			CommandContains: req.CommandContains, InventoryID: req.InventoryID,
			ExcludeDryRun: req.ExcludeDryRun, MaxDestroy: resolveMaxDestroy(req.MaxDestroy),
			CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), p); err != nil {
			log.Error("server: save policy: " + err.Error())
			if denyReadOnlyPolicies(w, log, err) {
				return
			}
			respondError(w, log, http.StatusInternalServerError, "could not store policy")
			return
		}
		respondJSON(w, log, http.StatusCreated, p, wantsPretty(r))
	}
}

// updatePolicyHandler replaces an existing approval policy, keeping its original creation time.
func updatePolicyHandler(store policy.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "policies not enabled")
			return
		}
		var req createPolicyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		if req.Tool != "" && !run.ValidTool(req.Tool) {
			respondError(w, log, http.StatusBadRequest, "tool is not a supported execution tool")
			return
		}
		id := r.PathValue("id")
		existing, err := store.Get(r.Context(), id)
		if errors.Is(err, policy.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "policy not found")
			return
		}
		if err != nil {
			log.Error("server: read policy: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read policy")
			return
		}
		p := &policy.Policy{
			ID: id, Name: req.Name, Tool: req.Tool,
			CommandContains: req.CommandContains, InventoryID: req.InventoryID,
			ExcludeDryRun: req.ExcludeDryRun, MaxDestroy: resolveMaxDestroy(req.MaxDestroy),
			CreatedAt: existing.CreatedAt,
		}
		if err := store.Save(r.Context(), p); err != nil {
			log.Error("server: update policy: " + err.Error())
			if denyReadOnlyPolicies(w, log, err) {
				return
			}
			respondError(w, log, http.StatusInternalServerError, "could not store policy")
			return
		}
		respondJSON(w, log, http.StatusOK, p, wantsPretty(r))
	}
}

// listPoliciesHandler returns all approval policies.
func listPoliciesHandler(store policy.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "policies not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list policies: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list policies")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listPoliciesResponse{Policies: list, Count: len(list)}, wantsPretty(r))
	}
}

// deletePolicyHandler removes an approval policy.
func deletePolicyHandler(store policy.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "policies not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, policy.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "policy not found")
			return
		}
		if err != nil {
			log.Error("server: delete policy: " + err.Error())
			if denyReadOnlyPolicies(w, log, err) {
				return
			}
			respondError(w, log, http.StatusInternalServerError, "could not delete policy")
			return
		}
		respondJSON(w, log, http.StatusOK,
			map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}

// denyReadOnlyPolicies reports whether err means the policy source refuses changes, and writes the
// reason when it does.
//
// A file-backed policy set is read-only on purpose: the file is the source of truth so a change to
// what needs approval goes through review. Reporting that as a 500 tells an operator something
// broke, when what actually happened is the system working as configured, so it is a conflict with
// an explanation instead.
func denyReadOnlyPolicies(w http.ResponseWriter, log *zap.Logger, err error) bool {
	if !errors.Is(err, policy.ErrReadOnly) {
		return false
	}
	respondError(w, log, http.StatusConflict, err.Error())
	return true
}
