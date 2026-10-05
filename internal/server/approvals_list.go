package server

import (
	"net/http"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// approvalUpstream is one step a waiting approval step waited behind, as its approver is shown it.
type approvalUpstream struct {
	// Name is the step's name.
	Name string `json:"name"`
	// Status is how the step ended, or skipped when it never ran.
	Status string `json:"status"`
	// RunID is the run the step executed as, empty when it never ran.
	RunID string `json:"run_id,omitempty"`
}

// approvalStepView is one workflow approval step waiting for a decision, with what an approver
// needs to decide it: what the workflow is, what already ran, and what each answer runs next.
type approvalStepView struct {
	// ID is the approval step's record, which approve and reject take.
	ID string `json:"id"`
	// RunID is the workflow run waiting at the step.
	RunID string `json:"run_id"`
	// Workflow is the workflow's name.
	Workflow string `json:"workflow"`
	// Step is the approval step's name.
	Step string `json:"step"`
	// Description is what the step's author asked the approver to decide.
	Description string `json:"description,omitempty"`
	// RequestedAt is when the workflow reached the step.
	RequestedAt time.Time `json:"requested_at"`
	// Timeout is how many seconds the step waits, zero for no limit.
	Timeout int `json:"timeout,omitempty"`
	// ExpiresAt is when the step times out and takes its deny path, absent when it never does.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// RequestedBy is who launched the workflow.
	RequestedBy string `json:"requested_by,omitempty"`
	// RequestedByType is how the launcher authenticated, such as agent or session.
	RequestedByType string `json:"requested_by_type,omitempty"`
	// RequireDistinctApprover says the launcher cannot approve this step.
	RequireDistinctApprover bool `json:"require_distinct_approver,omitempty"`
	// RequireReason says a decision on this step must carry the decider's reason: denials for a
	// denial, always for both.
	RequireReason string `json:"require_reason,omitempty"`
	// Upstream is every step the approval waited behind and how it ended.
	Upstream []approvalUpstream `json:"upstream,omitempty"`
	// OnApprove names the steps an approval runs.
	OnApprove []string `json:"on_approve,omitempty"`
	// OnDeny names the steps a denial or a timeout runs.
	OnDeny []string `json:"on_deny,omitempty"`
	// StateDigest is the digest of exactly this state. Sending it back with a decision binds the
	// decision to what was shown.
	StateDigest string `json:"state_digest"`
}

// approvalsResponse is the GET /approvals envelope.
type approvalsResponse struct {
	// Approvals are the waiting approval steps, oldest first.
	Approvals []approvalStepView `json:"approvals"`
	// Count is how many were returned.
	Count int `json:"count"`
	// Total is how many are waiting that the caller may see.
	Total int `json:"total"`
}

// approvalsHandler lists the workflow approval steps waiting for a decision that the caller may
// see. A workflow paused at a step is still running whenever another branch of it is, so the runs
// list held for approval cannot show it, and this is where an approver finds it. Reading the queue
// is a viewer read: an agent may see that its work waits, which is what keeps it from guessing, and
// still cannot decide anything, since deciding is an admin route.
func approvalsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: approvalsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rows, err := store.NonTerminal(ctx)
		if err != nil {
			log.Error("server: list approval steps: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list approval steps")
			return
		}
		parents := map[string]*run.Run{}
		out := []approvalStepView{}
		for _, node := range rows {
			if node.Kind != run.KindApproval || node.Status != run.StatusPendingApproval ||
				node.ParentID == nil {
				continue
			}
			if authz.authorizeRun(ctx, grant.AccessUse, node) != nil {
				continue
			}
			parent, ok := parents[*node.ParentID]
			if !ok {
				parent, err = store.Get(ctx, *node.ParentID)
				if err != nil {
					continue
				}
				parents[parent.ID] = parent
			}
			view, verr := approvalView(r, store, parent, node)
			if verr != nil {
				log.Error("server: describe approval step: " + verr.Error())
				continue
			}
			out = append(out, view)
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].RequestedAt.Equal(out[j].RequestedAt) {
				return out[i].RequestedAt.Before(out[j].RequestedAt)
			}
			return out[i].ID < out[j].ID
		})
		respondJSON(w, log, http.StatusOK,
			approvalsResponse{Approvals: out, Count: len(out), Total: len(out)}, wantsPretty(r))
	}
}

// approvalView describes one waiting step from its record and its workflow. Upstream outputs are
// left out: they can carry what a playbook published, and the digest already binds them.
func approvalView(r *http.Request, store run.Store, parent, node *run.Run) (approvalStepView, error) {
	state, err := outcome.StepStateOf(r.Context(), store, parent, node)
	if err != nil {
		return approvalStepView{}, err
	}
	digest, err := state.Digest()
	if err != nil {
		return approvalStepView{}, err
	}
	view := approvalStepView{
		ID: node.ID, RunID: parent.ID, Workflow: parent.Playbook, Step: node.StepName,
		RequestedAt: node.CreatedAt, Timeout: node.Timeout, RequestedBy: parent.Actor,
		RequestedByType: parent.ActorType, RequireDistinctApprover: node.RequireDistinctApprover,
		OnApprove: state.OnApprove, OnDeny: state.OnDeny, StateDigest: digest,
		RequireReason: node.RequireReason,
	}
	if node.StepIndex != nil && *node.StepIndex >= 0 && *node.StepIndex < len(parent.Steps) {
		view.Description = parent.Steps[*node.StepIndex].Description
	}
	if node.Timeout > 0 {
		at := node.CreatedAt.Add(time.Duration(node.Timeout) * time.Second)
		view.ExpiresAt = &at
	}
	for _, u := range state.Upstream {
		view.Upstream = append(view.Upstream, approvalUpstream{Name: u.Name, Status: u.Status,
			RunID: u.RunID})
	}
	return view, nil
}
