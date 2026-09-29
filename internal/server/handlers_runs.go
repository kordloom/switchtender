package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
)

// idempotencyKeyHeader carries a client-chosen key that dedupes a retried submission, so a dropped
// response or a client retry on POST /runs and POST /pipelines cannot double-fire a run.
const idempotencyKeyHeader = "Idempotency-Key"

// maxPipelineSteps bounds one pipeline, because the dispatcher's dependency closure is quadratic in
// the step count and the request body's byte cap is not a cap on that work.
const maxPipelineSteps = 500

// dedupeRerun names the rerun action in the idempotency keys it dedupes under, so a rerun the
// caller supplied no key for still cannot fire twice on a double click.
const dedupeRerun = "rerun"

// createRunRequest is the JSON body accepted by POST /runs.
type createRunRequest struct {
	// Playbook is the path to the playbook to execute. Required for the Ansible tool.
	Playbook string `json:"playbook"`
	// Inventory is the path to the inventory to target. Optional.
	Inventory string `json:"inventory"`
	// Limit narrows the run to a host pattern, the same field a template launch accepts. Without it
	// a caller asking to touch one canary host was answered 202 and run against every host in the
	// inventory, because an unknown JSON field is dropped rather than refused.
	Limit string `json:"limit,omitempty"`
	// Tool selects the execution engine: ansible (default), bash, terraform, or python.
	Tool string `json:"tool,omitempty"`
	// Command is the tool's input for non-Ansible tools: the script for bash and python, the working
	// directory for terraform. Required for those tools, ignored for Ansible.
	Command string `json:"command,omitempty"`
	// DryRun runs the tool in its no-change mode: ansible --check, a syntax check for bash.
	DryRun bool `json:"dry_run,omitempty"`
	// Tags runs only the Ansible plays and tasks carrying one of these tags. Ignored by other tools.
	Tags []string `json:"tags,omitempty"`
	// SkipTags skips the Ansible plays and tasks carrying one of these tags. Ignored by other tools.
	SkipTags []string `json:"skip_tags,omitempty"`
	// Verbosity raises Ansible logging from 0 to 4 for this run.
	Verbosity int `json:"verbosity,omitempty"`
	// Forks sets how many hosts Ansible addresses in parallel. Zero leaves Ansible's default.
	Forks int `json:"forks,omitempty"`
	// DiffMode shows the before-and-after of every Ansible file and template change.
	DiffMode bool `json:"diff_mode,omitempty"`
	// Labels are user-supplied key values attached to the run for slicing and audits.
	Labels map[string]string `json:"labels,omitempty"`
	// ExtraVars are the variables injected into the run, the same field a template carries and a
	// template launch overrides. A plugin tool reads them as its input, so dropping them ran the
	// tool with none of the configuration the caller sent and answered 202 as though it had.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Shards, when two or more, splits the run across that many inventory slices.
	Shards int `json:"shards,omitempty"`
	// CredentialIDs names stored credentials to materialize for the run.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ProjectID sources the playbook and inventory from a git project.
	ProjectID string `json:"project_id,omitempty"`
	// InventoryID targets a stored inventory instead of a path.
	InventoryID string `json:"inventory_id,omitempty"`
	// Queue restricts execution to workers serving the queue.
	Queue string `json:"queue,omitempty"`
	// Image names a container image the run executes inside, its execution environment. Any tool.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// RequireApproval holds the run for approval before it executes. Honored for a single run, not
	// a shard split.
	RequireApproval bool `json:"require_approval,omitempty"`
	// Timeout caps how many seconds the run may execute before it is canceled and failed. Zero uses
	// the server default.
	Timeout int `json:"timeout,omitempty"`
	// Notifications routes this run's terminal state to specific channels beyond the server-wide
	// ones.
	Notifications []run.NotifyTarget `json:"notifications,omitempty"`
}

// createPipelineRequest is the JSON body accepted by POST /pipelines.
type createPipelineRequest struct {
	// Name identifies the pipeline. Optional.
	Name string `json:"name"`
	// Inventory is the default inventory for steps that do not set their own. Optional.
	Inventory string `json:"inventory"`
	// Steps is the ordered list of steps to run. Required, at least one.
	Steps []run.PipelineStep `json:"steps"`
	// CredentialIDs names stored credentials to materialize for every step.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ProjectID sources every step's playbook from a git project.
	ProjectID string `json:"project_id,omitempty"`
	// Queue restricts execution to workers serving the queue.
	Queue string `json:"queue,omitempty"`
	// RequireApproval holds the pipeline for an approver before any step runs. A stored policy that
	// matches any step holds it regardless, so this only adds a gate rather than being the only one.
	RequireApproval bool `json:"require_approval,omitempty"`
}

// createRunHandler accepts a run request and submits it for execution. It authorizes the actor for
// every object the run references, so a run cannot borrow another user's project, inventory, or
// credentials to reach hosts they were never granted.
func createRunHandler(submitter Submitter, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if submitter == nil {
		panic("server: createRunHandler: Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Strict, like every other mutating endpoint. The controls here are the safety controls, so a
		// key this endpoint does not recognize is refused rather than dropped: a submission asking for
		// dry_run, require_approval, or limit by a name one character off was accepted, executed
		// without it, and answered with a body indistinguishable from the run that was asked for.
		var req createRunRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if !run.ValidTool(req.Tool) {
			respondError(w, log, http.StatusBadRequest,
				"tool must be ansible, bash, terraform, opentofu, python, powershell, or go")
			return
		}
		if run.NormalizeTool(req.Tool) == run.ToolAnsible {
			if req.Playbook == "" {
				respondError(w, log, http.StatusBadRequest, "playbook is required")
				return
			}
		} else if req.Command == "" {
			respondError(w, log, http.StatusBadRequest, "command is required for the "+req.Tool+" tool")
			return
		}

		objects := append([]string{req.ProjectID, req.InventoryID, req.PullCredentialID,
			queueObject(req.Queue)}, req.CredentialIDs...)
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objects...)) {
			return
		}

		var created *run.Run
		var err error
		opts := []run.SubmitOption{
			run.WithCredentialIDs(req.CredentialIDs),
			run.WithTool(req.Tool), run.WithCommand(req.Command), run.WithDryRun(req.DryRun),
			run.WithSource("api", ""), run.WithActor(actorName(r)),
			run.WithActorAccount(actorAccount(r)),
			run.WithActorType(actorType(r)), run.WithLabels(req.Labels),
			run.WithTags(req.Tags...), run.WithSkipTags(req.SkipTags...),
			run.WithVerbosity(req.Verbosity), run.WithForks(req.Forks), run.WithDiffMode(req.DiffMode),
			run.WithExtraVars(req.ExtraVars),
		}
		if supplied := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader)); supplied != "" {
			key, err := run.ClientKey(supplied, run.SubmitterOrgFrom(r.Context()))
			if err != nil {
				respondError(w, log, http.StatusBadRequest, err.Error())
				return
			}
			opts = append(opts, run.WithIdempotencyKey(key))
		}
		if req.ProjectID != "" {
			opts = append(opts, run.WithProject(req.ProjectID))
		}
		if req.InventoryID != "" {
			opts = append(opts, run.WithInventory(req.InventoryID))
		}
		if req.Queue != "" {
			opts = append(opts, run.WithQueue(req.Queue))
		}
		if req.Limit != "" {
			opts = append(opts, run.WithLimit(req.Limit))
		}
		if req.Image != "" {
			opts = append(opts, run.WithImage(req.Image, req.PullCredentialID))
		}
		if req.Timeout > 0 {
			opts = append(opts, run.WithTimeout(req.Timeout))
		}
		if err := run.ValidateNotifyTargets(req.Notifications); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		if len(req.Notifications) > 0 {
			opts = append(opts, run.WithNotifications(req.Notifications))
		}
		// The hold applies to a split too. Dropping it here meant asking for approval and asking for
		// shards in the same request silently got neither: the API answered 202 and the fan-out ran
		// on every host at once. A split stores its shards held alongside a held parent, so the
		// option needs nothing more than to be passed along.
		if req.RequireApproval {
			opts = append(opts, run.WithRequireApproval(true))
		}
		if req.Shards >= 2 {
			created, err = submitter.SubmitSplit(r.Context(), req.Playbook, req.Inventory,
				req.Shards, opts...)
		} else {
			created, err = submitter.Submit(r.Context(), req.Playbook, req.Inventory, opts...)
		}
		switch {
		case errors.Is(err, credential.ErrNotFound), errors.Is(err, credential.ErrNoKey),
			errors.Is(err, project.ErrNotFound), errors.Is(err, inventory.ErrNotFound),
			errors.Is(err, dispatch.ErrNoPlaybook), errors.Is(err, dispatch.ErrNoCommand),
			errors.Is(err, dispatch.ErrUnknownTool), errors.Is(err, dispatch.ErrToolCredential):
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, dispatch.ErrPolicyDenied):
			respondError(w, log, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Error("server: submit run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not submit run")
			return
		}

		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, maskRun(created), wantsPretty(r))
	}
}

// createPipelineHandler accepts a pipeline request and submits it for execution. Like a run, a
// pipeline reaches hosts through a project and credentials, so the actor must hold use access on
// each before it is submitted.
func createPipelineHandler(submitter Submitter, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if submitter == nil {
		panic("server: createPipelineHandler: Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req createPipelineRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if len(req.Steps) == 0 {
			respondError(w, log, http.StatusBadRequest, "at least one step is required")
			return
		}
		// The dependency closure the dispatcher builds is quadratic in the step count, and only the
		// zero case was rejected. Fifteen thousand steps fit in a one megabyte body, validated in
		// five milliseconds so the submission looked fine, and then cost the dispatcher hundreds of
		// megabytes at execution time. A pipeline is a handful of steps by nature.
		if len(req.Steps) > maxPipelineSteps {
			respondError(w, log, http.StatusBadRequest, fmt.Sprintf(
				"a pipeline may have at most %d steps", maxPipelineSteps))
			return
		}
		for _, step := range req.Steps {
			if !run.ValidTool(step.Tool) {
				respondError(w, log, http.StatusBadRequest,
					"each step tool must be ansible, bash, terraform, opentofu, python, powershell, or go")
				return
			}
			if run.NormalizeTool(step.Tool) == run.ToolAnsible && step.Playbook == "" {
				respondError(w, log, http.StatusBadRequest, "each ansible step requires a playbook")
				return
			}
			if run.NormalizeTool(step.Tool) != run.ToolAnsible && step.Command == "" {
				respondError(w, log, http.StatusBadRequest, "each non-ansible step requires a command")
				return
			}
		}

		objects := append([]string{req.ProjectID, queueObject(req.Queue)}, req.CredentialIDs...)
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objects...)) {
			return
		}

		popts := []run.SubmitOption{run.WithCredentialIDs(req.CredentialIDs),
			run.WithActor(actorName(r)), run.WithActorAccount(actorAccount(r)),
			run.WithActorType(actorType(r))}
		// Validated exactly as a run submission is. Taking the header verbatim here let a caller
		// mint a key in the reserved namespace that derived keys use, plant a run under the key a
		// webhook or a rerun would later compute, and have that later launch resolve to the planted
		// run and never execute. The git host is answered 202 and the deployment silently does not
		// happen, which is the worst shape a failure can take.
		if supplied := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader)); supplied != "" {
			key, err := run.ClientKey(supplied, run.SubmitterOrgFrom(r.Context()))
			if err != nil {
				respondError(w, log, http.StatusBadRequest, err.Error())
				return
			}
			popts = append(popts, run.WithIdempotencyKey(key))
		}
		if req.ProjectID != "" {
			popts = append(popts, run.WithProject(req.ProjectID))
		}
		if req.Queue != "" {
			popts = append(popts, run.WithQueue(req.Queue))
		}
		if req.RequireApproval {
			popts = append(popts, run.WithRequireApproval(true))
		}
		created, err := submitter.SubmitPipeline(r.Context(), req.Name, req.Inventory, req.Steps,
			popts...)
		switch {
		case errors.Is(err, credential.ErrNotFound), errors.Is(err, credential.ErrNoKey),
			errors.Is(err, project.ErrNotFound):
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, dispatch.ErrUnnamedStep), errors.Is(err, dispatch.ErrDuplicateStep),
			errors.Is(err, dispatch.ErrUnknownDependency), errors.Is(err, dispatch.ErrDependencyCycle),
			errors.Is(err, dispatch.ErrStepInput), errors.Is(err, dispatch.ErrTooManySteps),
			errors.Is(err, dispatch.ErrNoPlaybook), errors.Is(err, dispatch.ErrNoCommand),
			errors.Is(err, dispatch.ErrUnknownTool), errors.Is(err, dispatch.ErrToolCredential):
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, dispatch.ErrPolicyDenied):
			respondError(w, log, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Error("server: submit pipeline: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not submit pipeline")
			return
		}
		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, maskRun(created), wantsPretty(r))
	}
}

// cancelRunHandler stops a pending or executing run. The cancel request persists in the store so
// the process holding the run honors it even when that is not this one; a local cancel is also
// attempted for an immediate stop.
func cancelRunHandler(store run.Store, canceler Canceler, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: cancelRunHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		existing, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: cancel run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not cancel run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, existing) {
			return
		}
		if existing.Status.Terminal() {
			respondError(w, log, http.StatusConflict, "run already finished")
			return
		}
		// A run no executor holds yet is terminalized directly: no process would ever act on the
		// cooperative flag, so without this the run stayed claimable and could still launch.
		if existing.ClaimedBy == "" &&
			(existing.Status == run.StatusPending || existing.Status == run.StatusPendingApproval) {
			if done, err := store.CancelPending(r.Context(), id); err == nil && done {
				// A split stores its shards alongside the parent, so canceling the parent has to
				// settle them too. Rejecting a split already did this; canceling did not, and the
				// store sweep cannot cover it either, because orphan resolution only fires for an
				// interrupted parent and a canceled one is terminal. The shards sat awaiting an
				// approval that would never come, and approving one ran it under a canceled
				// parent with nothing to roll it up.
				cancelChildrenOf(r.Context(), store, log, existing)
				respondJSON(w, log, http.StatusAccepted,
					map[string]string{"status": "canceled"}, wantsPretty(r))
				return
			}
			// Lost the race to a claim or the store cannot do it; fall through to the
			// cooperative path.
		}
		if err := store.RequestCancel(r.Context(), id); err != nil {
			log.Error("server: cancel run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not cancel run")
			return
		}
		if canceler != nil {
			canceler.Cancel(id)
		}
		respondJSON(w, log, http.StatusAccepted,
			map[string]string{"status": "canceling"}, wantsPretty(r))
	}
}

// retryRunHandler starts a new split run from the failed shards of a finished one.
func retryRunHandler(store run.Store, retrier Retrier, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: retryRunHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if retrier == nil {
			respondError(w, log, http.StatusNotFound, "retry not enabled")
			return
		}
		id := r.PathValue("id")
		rn, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: retry run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not retry run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		created, err := retrier.RetryFailedShards(r.Context(), id)
		switch {
		case errors.Is(err, run.ErrNotFound):
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		case errors.Is(err, dispatch.ErrNotSplit):
			respondError(w, log, http.StatusConflict, "only split runs can retry failed shards")
			return
		case errors.Is(err, dispatch.ErrNotFinished):
			respondError(w, log, http.StatusConflict, "run has not finished")
			return
		case errors.Is(err, dispatch.ErrNoFailedShards):
			respondError(w, log, http.StatusConflict, "no failed shards to retry")
			return
		case errors.Is(err, dispatch.ErrPolicyDenied):
			respondError(w, log, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Error("server: retry run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not retry run")
			return
		}
		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, maskRun(created), wantsPretty(r))
	}
}

// relaunchFailedHandler re-runs a finished run against only the hosts that failed or were
// unreachable, linking the new run back to the one it was built to fix. It is operator work, the
// same role that launches a run, and every derivation lands in the audit chain.
func relaunchFailedHandler(store run.Store, retrier Retrier, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: relaunchFailedHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if retrier == nil {
			respondError(w, log, http.StatusNotFound, "relaunch not enabled")
			return
		}
		id := r.PathValue("id")
		rn, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: relaunch failed hosts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not relaunch run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		created, err := retrier.RelaunchFailedHosts(r.Context(), id, actorName(r), actorType(r))
		switch {
		case errors.Is(err, run.ErrNotFound):
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		case errors.Is(err, dispatch.ErrNotFinished):
			respondError(w, log, http.StatusConflict, "run has not finished")
			return
		case errors.Is(err, dispatch.ErrNoHostSummary):
			respondError(w, log, http.StatusConflict,
				"this run recorded no per-host results, so it has no failed hosts to relaunch")
			return
		case errors.Is(err, dispatch.ErrNoFailedHosts):
			respondError(w, log, http.StatusConflict, "no hosts failed, so there is nothing to relaunch")
			return
		case errors.Is(err, dispatch.ErrPolicyDenied):
			respondError(w, log, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Error("server: relaunch failed hosts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not relaunch run")
			return
		}
		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, maskRun(created), wantsPretty(r))
	}
}

// cancelChildrenOf settles the children stored with a parent that was canceled before it started.
//
// A child that cannot be settled is logged rather than failing the cancel: the parent is what the
// caller asked to stop, and it is already stopped.
func cancelChildrenOf(ctx context.Context, store run.Store, log *zap.Logger, parent *run.Run) {
	if parent.Kind != run.KindSplit && parent.Kind != run.KindPipeline {
		return
	}
	children, err := store.Shards(ctx, parent.ID)
	if err != nil {
		log.Error("server: list shards of a canceled run: " + err.Error())
		return
	}
	for _, c := range children {
		if c.Status.Terminal() {
			continue
		}
		if done, cerr := store.CancelPending(ctx, c.ID); cerr != nil {
			log.Error("server: cancel shard " + c.ID + ": " + cerr.Error())
		} else if !done {
			// Already claimed by an executor, so it stops cooperatively instead.
			if rerr := store.RequestCancel(ctx, c.ID); rerr != nil {
				log.Error("server: request cancel of shard " + c.ID + ": " + rerr.Error())
			}
		}
	}
}

// rerunOptions rebuilds the submit options a stored run was created with, so a rerun replays the
// full spec: everything in the run's execution options, plus its host limit, labels, and
// notification targets.
func rerunOptions(rn *run.Run) []run.SubmitOption {
	// A rerun is the same run again, so it starts from the run's own execution spec. Keeping a
	// separate list here is what lost it the timeout and the notifications: the run would execute
	// under the dispatcher's default cap instead of its own, and its terminal state would reach the
	// server-wide channels but not the team the original run paged.
	opts := rn.ExecutionOptions()
	// What a rerun adds on top is what belongs to the run rather than to how it executes. A shard
	// owns its Limit, so ExecutionOptions leaves it out, but a rerun of a plain run replays the
	// host pattern the operator chose.
	if rn.Limit != "" {
		opts = append(opts, run.WithLimit(rn.Limit))
	}
	if len(rn.Labels) > 0 {
		opts = append(opts, run.WithLabels(rn.Labels))
	}
	if len(rn.Notifications) > 0 {
		opts = append(opts, run.WithNotifications(rn.Notifications))
	}
	return opts
}

// rerunRefusal reports why a finished run must not be replayed, or an empty string when it may be.
//
// The two cases are decisions rather than outcomes. A rejected run was denied by an approver, which
// the retry path has always refused to replay. A run canceled before it started was withdrawn
// before anyone let it run. Rerunning either turns a recorded decision into a fresh, ungated run,
// because the replay carries the execution spec and not the hold that was on it.
func rerunRefusal(rn *run.Run) string {
	switch {
	case rn.Status == run.StatusRejected:
		return "this run was rejected, so it cannot be run again from here: submit a new run if " +
			"it should be reconsidered"
	case rn.Status == run.StatusCanceled && rn.StartedAt == nil:
		return "this run was canceled before it started, so it cannot be run again from here: " +
			"submit a new run if it should be reconsidered"
	default:
		return ""
	}
}

// rerunRunHandler starts a fresh run with the same spec as a finished one. A split parent reruns
// as a new split. Pipeline parents and shard or step children are refused, since their spec lives
// with the workflow or the parent.
func rerunRunHandler(store run.Store, submitter Submitter, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: rerunRunHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if submitter == nil {
			respondError(w, log, http.StatusNotFound, "rerun not enabled")
			return
		}
		rn, err := store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: rerun: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not rerun")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		if rn.ParentID != nil {
			respondError(w, log, http.StatusConflict, "rerun the parent run instead of a shard or step")
			return
		}
		if rn.Kind == run.KindPipeline {
			respondError(w, log, http.StatusConflict, "rerun the pipeline from its workflow instead")
			return
		}
		if !rn.Status.Terminal() {
			respondError(w, log, http.StatusConflict, "run has not finished")
			return
		}
		// A rerun replays a spec, and it must not replay past a decision that the spec should not
		// run. A rejected run is one an approver denied. A run canceled before it ever started is
		// one somebody withdrew. Neither ever executed, and the replay drops the hold that was on
		// them: the fresh run is gated only by a stored policy, so a hold that came from
		// require_approval, a drift reconcile, or a generated proposal was silently discarded and
		// the denied command ran from a one-click button on the denied run's own page.
		if reason := rerunRefusal(rn); reason != "" {
			respondError(w, log, http.StatusConflict, reason)
			return
		}
		// Access to the run is not enough to fire its spec again. Authorize every object the new
		// run touches, mirroring a template launch, so a rerun cannot borrow a project,
		// inventory, or credential the actor was never granted.
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, runObjects(rn)...)) {
			return
		}
		// Rerunning the same run twice inside the dedupe window is one request, not two, so a
		// double click returns the run the first click started rather than firing a second.
		existing, key, err := run.ResolveDedupe(r.Context(), store, dedupeRerun, rn.ID, time.Now())
		if err != nil {
			log.Error("server: rerun: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not rerun")
			return
		}
		if existing != nil {
			w.Header().Set("Location", "/v1/runs/"+existing.ID)
			respondJSON(w, log, http.StatusAccepted, maskRun(existing), wantsPretty(r))
			return
		}
		opts := append(rerunOptions(rn), run.WithSource("rerun", rn.ID), run.WithRerunOf(rn.ID),
			run.WithActor(actorName(r)), run.WithActorAccount(actorAccount(r)),
			run.WithActorType(actorType(r)),
			run.WithIdempotencyKey(key))
		var created *run.Run
		if rn.Kind == run.KindSplit && rn.ShardCount != nil && *rn.ShardCount > 1 {
			created, err = submitter.SubmitSplit(r.Context(), rn.Playbook, rn.Inventory, *rn.ShardCount, opts...)
		} else {
			created, err = submitter.Submit(r.Context(), rn.Playbook, rn.Inventory, opts...)
		}
		if errors.Is(err, dispatch.ErrPolicyDenied) {
			respondError(w, log, http.StatusForbidden, err.Error())
			return
		}
		if err != nil {
			log.Error("server: rerun: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not rerun")
			return
		}
		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, maskRun(created), wantsPretty(r))
	}
}
