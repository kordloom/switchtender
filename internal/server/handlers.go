package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// defaultFleetWindow is the number of recent runs per host considered when no window is given.
const defaultFleetWindow = 10

// idempotencyKeyHeader carries a client-chosen key that dedupes a retried submission, so a dropped
// response or a client retry on POST /runs and POST /pipelines cannot double-fire a run.
const idempotencyKeyHeader = "Idempotency-Key"

// createRunRequest is the JSON body accepted by POST /runs.
type createRunRequest struct {
	// Playbook is the path to the playbook to execute. Required for the Ansible tool.
	Playbook string `json:"playbook"`
	// Inventory is the path to the inventory to target. Optional.
	Inventory string `json:"inventory"`
	// Tool selects the execution engine: ansible (default), bash, terraform, or python.
	Tool string `json:"tool,omitempty"`
	// Command is the tool's input for non-Ansible tools: the script for bash and python, the working
	// directory for terraform. Required for those tools, ignored for Ansible.
	Command string `json:"command,omitempty"`
	// DryRun runs the tool in its no-change mode: ansible --check, a syntax check for bash.
	DryRun bool `json:"dry_run,omitempty"`
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
	// Image names a container image the run executes inside, its execution environment. Ansible only.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// RequireApproval holds the run for approval before it executes. Honored for a single run, not
	// a shard split.
	RequireApproval bool `json:"require_approval,omitempty"`
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
}

// listRunsResponse wraps a run list. The envelope leaves room for pagination fields later.
type listRunsResponse struct {
	// Runs is the ordered list of runs.
	Runs []*run.Run `json:"runs"`
	// Count is the number of runs returned.
	Count int `json:"count"`
	// Summary is the run totals across every page, for the summary cards.
	Summary runSummary `json:"summary"`
	// HasMore reports whether another page follows this one.
	HasMore bool `json:"has_more"`
}

// runSummary is the per-status rollup of all top-level runs, shown as cards above the list.
type runSummary struct {
	// Total is the number of top-level runs.
	Total int `json:"total"`
	// Succeeded is how many finished successfully.
	Succeeded int `json:"succeeded"`
	// Failed is how many failed.
	Failed int `json:"failed"`
	// Active is how many are running or pending.
	Active int `json:"active"`
}

// summarize folds status counts into the summary the runs view shows.
func summarize(counts map[run.Status]int) runSummary {
	s := runSummary{}
	for status, n := range counts {
		s.Total += n
		switch status {
		case run.StatusSucceeded:
			s.Succeeded += n
		case run.StatusFailed:
			s.Failed += n
		case run.StatusRunning, run.StatusPending:
			s.Active += n
		}
	}
	return s
}

// eventsResponse wraps a run's structured events.
type eventsResponse struct {
	// Events is the ordered list of events.
	Events []event.Event `json:"events"`
	// Count is the number of events returned.
	Count int `json:"count"`
	// NextAfter is the sequence cursor to pass back as ?after= to page the events that
	// follow this batch. It is the last event's Seq, or the requested after when empty.
	NextAfter int64 `json:"next_after"`
}

// shardsResponse wraps a parent run's shard runs.
type shardsResponse struct {
	// Shards is the ordered list of shard runs.
	Shards []*run.Run `json:"shards"`
	// Count is the number of shards returned.
	Count int `json:"count"`
}

// stepsResponse wraps a pipeline run's step runs.
type stepsResponse struct {
	// Steps is the ordered list of step runs.
	Steps []*run.Run `json:"steps"`
	// Count is the number of steps returned.
	Count int `json:"count"`
}

// fleetResponse wraps the fleet health ranking.
type fleetResponse struct {
	// Hosts is the ranking of hosts by recent failures, worst first.
	Hosts []run.HostHealth `json:"hosts"`
	// Count is the number of hosts returned.
	Count int `json:"count"`
	// Window is the number of recent runs per host considered.
	Window int `json:"window"`
}

// hostHistoryResponse wraps one host's recent per run outcomes.
type hostHistoryResponse struct {
	// Host is the target host.
	Host string `json:"host"`
	// Runs is the host's recent summaries, newest first.
	Runs []run.HostSummary `json:"runs"`
	// Count is the number of summaries returned.
	Count int `json:"count"`
}

// taskTrendsResponse wraps the task duration trends.
type taskTrendsResponse struct {
	// Tasks is the per task aggregate over recent runs.
	Tasks []run.TaskTrend `json:"tasks"`
	// Count is the number of tasks returned.
	Count int `json:"count"`
	// Window is the number of recent runs per task considered.
	Window int `json:"window"`
}

// fleetHandler ranks hosts by recent failures across runs.
func fleetHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: fleetHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		window := defaultFleetWindow
		if v := r.URL.Query().Get("window"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				window = n
			}
		}
		hosts, err := store.FleetHealth(r.Context(), window)
		if err != nil {
			log.Error("server: fleet health: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute fleet health")
			return
		}
		respondJSON(w, log, http.StatusOK,
			fleetResponse{Hosts: hosts, Count: len(hosts), Window: window}, wantsPretty(r))
	}
}

// driftResponse wraps the fleet drift status.
type driftResponse struct {
	// Hosts is each host's latest drift check, worst drift first.
	Hosts []run.HostDrift `json:"hosts"`
	// Count is the number of hosts returned.
	Count int `json:"count"`
}

// driftHandler reports each host's most recent drift check, from the latest dry run to touch it.
func driftHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: driftHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		hosts, err := store.DriftStatus(r.Context())
		if err != nil {
			log.Error("server: drift status: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute drift status")
			return
		}
		respondJSON(w, log, http.StatusOK,
			driftResponse{Hosts: hosts, Count: len(hosts)}, wantsPretty(r))
	}
}

// reconcileRequest is the body of POST /drift/reconcile.
type reconcileRequest struct {
	// Host is the drifted host to build a reconcile proposal for.
	Host string `json:"host"`
}

// reconcileDriftHandler builds a reconcile proposal for a drifted target from the check run that
// observed the drift, run for real instead of in check mode: an Ansible check reruns its playbook
// limited to the drifted host, and a Terraform or OpenTofu check applies its working directory. The
// proposal is deterministic, no model constructs it, and it is born held for approval, so a person
// releases it or it never executes. The actor must hold use on every object the run will touch,
// exactly as a template launch requires.
func reconcileDriftHandler(store run.Store, submitter Submitter, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil || submitter == nil {
		panic("server: reconcileDriftHandler: Store and Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req reconcileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid json body")
			return
		}
		host := strings.TrimSpace(req.Host)
		if host == "" {
			respondError(w, log, http.StatusBadRequest, "a host is required")
			return
		}
		drift, err := store.DriftStatus(r.Context())
		if err != nil {
			log.Error("server: reconcile drift status: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute drift status")
			return
		}
		var entry *run.HostDrift
		for i := range drift {
			if drift[i].Host == host {
				entry = &drift[i]
				break
			}
		}
		if entry == nil {
			respondError(w, log, http.StatusNotFound, "no drift check recorded for that host")
			return
		}
		if entry.DriftedTasks == 0 {
			respondError(w, log, http.StatusConflict, "host is in sync")
			return
		}
		check, err := store.Get(r.Context(), entry.RunID)
		if err != nil {
			log.Error("server: reconcile check run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the check run")
			return
		}
		tool := run.NormalizeTool(check.Tool)
		if tool != run.ToolAnsible && tool != run.ToolTerraform && tool != run.ToolOpenTofu {
			respondError(w, log, http.StatusBadRequest,
				"reconcile is defined for ansible, terraform, and opentofu drift checks")
			return
		}

		// Authorize every object the proposal will touch, so a reconcile cannot borrow a project,
		// inventory, or credentials the actor was never granted.
		objects := append([]string{check.ProjectID, check.InventoryID}, check.CredentialIDs...)
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objects...)) {
			return
		}

		opts := []run.SubmitOption{
			run.WithTool(check.Tool),
			run.WithRequireApproval(true),
			run.WithProposedFrom(check.ID),
		}
		if tool == run.ToolAnsible {
			// The Ansible fix reruns the playbook limited to the drifted host, applying exactly the
			// divergent tasks.
			opts = append(opts, run.WithLimit(host))
		} else {
			// The Terraform or OpenTofu fix applies the drifted working directory for real.
			opts = append(opts, run.WithCommand(check.Command))
		}
		if check.ProjectID != "" {
			opts = append(opts, run.WithProject(check.ProjectID))
		}
		if check.InventoryID != "" {
			opts = append(opts, run.WithInventory(check.InventoryID))
		}
		if len(check.CredentialIDs) > 0 {
			opts = append(opts, run.WithCredentialIDs(check.CredentialIDs))
		}
		if len(check.ExtraVars) > 0 {
			opts = append(opts, run.WithExtraVars(check.ExtraVars))
		}
		if check.Queue != "" {
			opts = append(opts, run.WithQueue(check.Queue))
		}
		proposal, err := submitter.Submit(r.Context(), check.Playbook, check.Inventory, opts...)
		if err != nil {
			log.Error("server: reconcile submit: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create the proposal")
			return
		}
		respondJSON(w, log, http.StatusAccepted, proposal, wantsPretty(r))
	}
}

// hostHistoryHandler returns one host's recent per run outcomes, newest first.
func hostHistoryHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: hostHistoryHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.PathValue("host")
		limit := defaultFleetWindow
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		history, err := store.HostHistory(r.Context(), host, limit)
		if err != nil {
			log.Error("server: host history: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read host history")
			return
		}
		respondJSON(w, log, http.StatusOK,
			hostHistoryResponse{Host: host, Runs: history, Count: len(history)}, wantsPretty(r))
	}
}

// taskTrendsHandler returns per task duration aggregates over recent runs.
func taskTrendsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: taskTrendsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		window := defaultFleetWindow
		if v := r.URL.Query().Get("window"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				window = n
			}
		}
		tasks, err := store.TaskTrends(r.Context(), window)
		if err != nil {
			log.Error("server: task trends: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute task trends")
			return
		}
		respondJSON(w, log, http.StatusOK,
			taskTrendsResponse{Tasks: tasks, Count: len(tasks), Window: window}, wantsPretty(r))
	}
}

// workersResponse wraps the executor list.
type workersResponse struct {
	// Workers is the list of executors, most recently seen first.
	Workers []run.WorkerInfo `json:"workers"`
	// Count is the number returned.
	Count int `json:"count"`
}

// workersHandler lists the fleet's executors from the leases they hold.
func workersHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: workersHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		workers, err := store.Workers(r.Context())
		if err != nil {
			log.Error("server: list workers: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list workers")
			return
		}
		respondJSON(w, log, http.StatusOK,
			workersResponse{Workers: workers, Count: len(workers)}, wantsPretty(r))
	}
}

// healthHandler reports service liveness.
func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, zap.NewNop(), http.StatusOK, map[string]string{"status": "ok"}, false)
	}
}

// createRunHandler accepts a run request and submits it for execution. It authorizes the actor for
// every object the run references, so a run cannot borrow another user's project, inventory, or
// credentials to reach hosts they were never granted.
func createRunHandler(submitter Submitter, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if submitter == nil {
		panic("server: createRunHandler: Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req createRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
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

		objects := append([]string{req.ProjectID, req.InventoryID, req.PullCredentialID}, req.CredentialIDs...)
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objects...)) {
			return
		}

		var created *run.Run
		var err error
		opts := []run.SubmitOption{
			run.WithCredentialIDs(req.CredentialIDs),
			run.WithTool(req.Tool), run.WithCommand(req.Command), run.WithDryRun(req.DryRun),
		}
		if key := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader)); key != "" {
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
		if req.Image != "" {
			opts = append(opts, run.WithImage(req.Image, req.PullCredentialID))
		}
		if req.Shards >= 2 {
			created, err = submitter.SubmitSplit(r.Context(), req.Playbook, req.Inventory,
				req.Shards, opts...)
		} else {
			if req.RequireApproval {
				opts = append(opts, run.WithRequireApproval(true))
			}
			created, err = submitter.Submit(r.Context(), req.Playbook, req.Inventory, opts...)
		}
		switch {
		case errors.Is(err, credential.ErrNotFound), errors.Is(err, credential.ErrNoKey),
			errors.Is(err, project.ErrNotFound), errors.Is(err, inventory.ErrNotFound),
			errors.Is(err, dispatch.ErrNoPlaybook), errors.Is(err, dispatch.ErrNoCommand),
			errors.Is(err, dispatch.ErrUnknownTool), errors.Is(err, dispatch.ErrToolCredential):
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			log.Error("server: submit run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not submit run")
			return
		}

		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(req.Steps) == 0 {
			respondError(w, log, http.StatusBadRequest, "at least one step is required")
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

		objects := append([]string{req.ProjectID}, req.CredentialIDs...)
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objects...)) {
			return
		}

		popts := []run.SubmitOption{run.WithCredentialIDs(req.CredentialIDs)}
		if key := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader)); key != "" {
			popts = append(popts, run.WithIdempotencyKey(key))
		}
		if req.ProjectID != "" {
			popts = append(popts, run.WithProject(req.ProjectID))
		}
		if req.Queue != "" {
			popts = append(popts, run.WithQueue(req.Queue))
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
			errors.Is(err, dispatch.ErrNoPlaybook), errors.Is(err, dispatch.ErrNoCommand),
			errors.Is(err, dispatch.ErrUnknownTool), errors.Is(err, dispatch.ErrToolCredential):
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			log.Error("server: submit pipeline: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not submit pipeline")
			return
		}
		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
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
		case err != nil:
			log.Error("server: retry run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not retry run")
			return
		}
		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
	}
}

// approveRunHandler releases a run held for approval so it can execute.
func approveRunHandler(approver Approver, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if approver == nil {
			respondError(w, log, http.StatusNotFound, "approvals not enabled")
			return
		}
		created, err := approver.Approve(r.Context(), r.PathValue("id"))
		switch {
		case errors.Is(err, run.ErrNotFound):
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		case errors.Is(err, dispatch.ErrNotPendingApproval):
			respondError(w, log, http.StatusConflict, "run is not awaiting approval")
			return
		case err != nil:
			log.Error("server: approve run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not approve run")
			return
		}
		respondJSON(w, log, http.StatusOK, created, wantsPretty(r))
	}
}

// rejectRunHandler denies a run held for approval, recording an optional reason as its error.
func rejectRunHandler(approver Approver, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if approver == nil {
			respondError(w, log, http.StatusNotFound, "approvals not enabled")
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		created, err := approver.Reject(r.Context(), r.PathValue("id"), req.Reason)
		switch {
		case errors.Is(err, run.ErrNotFound):
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		case errors.Is(err, dispatch.ErrNotPendingApproval):
			respondError(w, log, http.StatusConflict, "run is not awaiting approval")
			return
		case err != nil:
			log.Error("server: reject run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not reject run")
			return
		}
		respondJSON(w, log, http.StatusOK, created, wantsPretty(r))
	}
}

// defaultRunsPage is the page size when a runs listing names none, and maxRunsPage is the largest
// page a caller can request, so one request can never materialize the whole run history.
const (
	defaultRunsPage = 200
	maxRunsPage     = 1000
)

// listRunsHandler returns a page of runs newest first, bounded even when no limit is given.
func listRunsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: listRunsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		limit := queryInt(r, "limit")
		if limit <= 0 {
			limit = defaultRunsPage
		}
		limit = min(limit, maxRunsPage)
		offset := queryInt(r, "offset")
		query := r.URL.Query().Get("q")
		runs, err := store.ListPage(r.Context(), query, limit, offset)
		if err != nil {
			log.Error("server: list runs: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		counts, err := store.RunStatusCounts(r.Context())
		if err != nil {
			log.Error("server: run status counts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		respondJSON(w, log, http.StatusOK, listRunsResponse{
			Runs:    runs,
			Count:   len(runs),
			Summary: summarize(counts),
			HasMore: len(runs) == limit,
		}, wantsPretty(r))
	}
}

// getRunHandler returns a single run by id.
func getRunHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: getRunHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got, err := store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			log.Error("server: get run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, got) {
			return
		}
		respondJSON(w, log, http.StatusOK, got, wantsPretty(r))
	}
}

// runShardsHandler returns the shard runs of a parent run.
func runShardsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runShardsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: list shards: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list shards")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		shards, err := store.Shards(r.Context(), id)
		if err != nil {
			log.Error("server: list shards: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list shards")
			return
		}
		respondJSON(w, log, http.StatusOK,
			shardsResponse{Shards: shards, Count: len(shards)}, wantsPretty(r))
	}
}

// runStepsHandler returns the step runs of a pipeline run.
func runStepsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runStepsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: list steps: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list steps")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		steps, err := store.Steps(r.Context(), id)
		if err != nil {
			log.Error("server: list steps: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list steps")
			return
		}
		respondJSON(w, log, http.StatusOK,
			stepsResponse{Steps: steps, Count: len(steps)}, wantsPretty(r))
	}
}

// runLogsHandler returns a run's captured output as plain text.
func runLogsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runLogsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, gerr := store.Get(r.Context(), id)
		if errors.Is(gerr, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if gerr != nil {
			log.Error("server: get run log: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run log")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		// The log streams to the client in chunk pages, so a multi-gigabyte log download never
		// materializes in the control plane's memory.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		var after int64
		for {
			chunks, err := store.LogAfter(r.Context(), id, after, streamBatch)
			if err != nil {
				log.Error("server: get run log: " + err.Error())
				return
			}
			for _, c := range chunks {
				after = c.Seq
				if _, err := w.Write(c.Data); err != nil {
					log.Error("server: write run log: " + err.Error())
					return
				}
			}
			if len(chunks) < streamBatch {
				return
			}
		}
	}
}

// defaultEventsPage is the page size when an events read names none, and maxEventsPage is the
// largest page a caller can request; the response's next_after cursor pages through the rest.
const (
	defaultEventsPage = 5000
	maxEventsPage     = 20000
)

// runEventsHandler returns a page of a run's structured events as JSON with a next_after cursor.
func runEventsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runEventsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, gerr := store.Get(r.Context(), id)
		if errors.Is(gerr, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if gerr != nil {
			log.Error("server: get run events: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run events")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		after := queryInt64(r, "after")
		limit := queryInt(r, "limit")
		if limit <= 0 {
			limit = defaultEventsPage
		}
		limit = min(limit, maxEventsPage)
		events, err := store.EventsAfter(r.Context(), id, after, limit)
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			log.Error("server: get run events: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run events")
			return
		}
		next := after
		if n := len(events); n > 0 {
			next = events[n-1].Seq
		}
		respondJSON(w, log, http.StatusOK,
			eventsResponse{Events: events, Count: len(events), NextAfter: next}, wantsPretty(r))
	}
}

// streamPollInterval is how often the stream handler drains new events from the store when no
// in-process signal arrives, which is how runs executing on other processes stream live.
const streamPollInterval = time.Second

// streamBatch caps how many events one drain reads from the store at a time, so a burst of
// output on a large run is emitted in bounded chunks rather than one unbounded read.
const streamBatch = 1000

// queryInt returns the named query parameter as a non-negative int, or zero when it is
// absent or not a positive number.
func queryInt(r *http.Request, name string) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// queryInt64 returns the named query parameter as a non-negative int64, or zero when it is
// absent or not a positive number.
func queryInt64(r *http.Request, name string) int64 {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// runStreamHandler streams a run's live events and log over Server Sent Events. The store is the
// source of truth: new rows beyond what the client has seen are emitted on a poll tick, and hub
// messages from a local executor only wake the drain early. Runs executing on any process in the
// fleet therefore stream the same way, and the stream ends when the stored run turns terminal.
func runStreamHandler(streamer Streamer, store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runStreamHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			respondError(w, log, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		id := r.PathValue("id")
		rn, gerr := store.Get(r.Context(), id)
		if errors.Is(gerr, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if gerr != nil {
			log.Error("server: stream run: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}

		var wake <-chan live.Message
		if streamer != nil {
			ch, cancel := streamer.Subscribe(id)
			defer cancel()
			wake = ch
		}

		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// The browser passes the seq of the last event it already has as ?after=, so the
		// stream resumes from there and never replays history. Without it, the stream starts
		// from the current end. On an automatic reconnect the browser sends Last-Event-ID
		// carrying both cursors, so a dropped connection resumes events and log bytes without
		// a gap. Each drain is an indexed range scan from its cursor, never a re-read of what
		// was already sent.
		var lastSeq int64
		if _, ok := r.URL.Query()["after"]; ok {
			lastSeq = queryInt64(r, "after")
		} else if seq, err := store.LastEventSeq(r.Context(), id); err == nil {
			lastSeq = seq
		}
		var logSeq int64
		if seq, err := store.LastLogSeq(r.Context(), id); err == nil {
			logSeq = seq
		}
		if ev, lg, ok := parseStreamCursor(r.Header.Get("Last-Event-ID")); ok {
			lastSeq, logSeq = ev, lg
		}

		drain := func() bool {
			for {
				evs, err := store.EventsAfter(r.Context(), id, lastSeq, streamBatch)
				if err != nil || len(evs) == 0 {
					break
				}
				for _, e := range evs {
					lastSeq = e.Seq
					data, err := json.Marshal(e)
					if err != nil {
						continue
					}
					writeSSE(w, "event", streamCursor(lastSeq, logSeq), data)
				}
				if len(evs) < streamBatch {
					break
				}
			}
			for {
				chunks, err := store.LogAfter(r.Context(), id, logSeq, streamBatch)
				if err != nil || len(chunks) == 0 {
					break
				}
				var buf []byte
				for _, c := range chunks {
					logSeq = c.Seq
					buf = append(buf, c.Data...)
				}
				if data, err := json.Marshal(string(buf)); err == nil {
					writeSSE(w, "log", streamCursor(lastSeq, logSeq), data)
				}
				if len(chunks) < streamBatch {
					break
				}
			}
			flusher.Flush()
			rn, err := store.Get(r.Context(), id)
			return err == nil && rn.Status.Terminal()
		}

		ticker := time.NewTicker(streamPollInterval)
		defer ticker.Stop()
		for {
			if drain() {
				writeSSE(w, "end", streamCursor(lastSeq, logSeq), nil)
				flusher.Flush()
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-wake:
			case <-ticker.C:
			}
		}
	}
}

// streamCursor encodes the event and log positions as one SSE id, the value a browser echoes back
// as Last-Event-ID when it reconnects.
func streamCursor(eventSeq, logSeq int64) string {
	return strconv.FormatInt(eventSeq, 10) + ":" + strconv.FormatInt(logSeq, 10)
}

// parseStreamCursor decodes a Last-Event-ID header written by streamCursor. ok is false for an
// absent or malformed value, leaving the caller's defaults in place.
func parseStreamCursor(v string) (eventSeq, logSeq int64, ok bool) {
	evPart, lgPart, found := strings.Cut(v, ":")
	if !found {
		return 0, 0, false
	}
	ev, err := strconv.ParseInt(evPart, 10, 64)
	if err != nil || ev < 0 {
		return 0, 0, false
	}
	lg, err := strconv.ParseInt(lgPart, 10, 64)
	if err != nil || lg < 0 {
		return 0, 0, false
	}
	return ev, lg, true
}

// writeSSE writes one Server Sent Event with the given event name, resume id, and JSON data. A
// write failure means the client went away; the stream loop ends on the closed request context.
func writeSSE(w http.ResponseWriter, name, id string, data []byte) {
	if len(data) == 0 {
		data = []byte("null")
	}
	_, _ = fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", name, id, data)
}
