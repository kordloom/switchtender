package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// summaryWindow returns the window the caller asked for in param, clamped to [1, hardCap], or the
// default when the parameter is absent or does not parse.
//
// The clamp is the point. The summary tables are the ones retention keeps rather than deletes, so
// they hold a row per host per run for the life of the fleet, and the fleet views turn every row a
// window admits into an element of the answer. Left uncapped, one query string could ask a single
// request to rank, concatenate and serialize the whole history of every host.
func summaryWindow(r *http.Request, param string, hardCap int) int {
	v := r.URL.Query().Get(param)
	if v == "" {
		return defaultFleetWindow
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return defaultFleetWindow
	}
	return min(n, hardCap)
}

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
func fleetHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: fleetHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		window := summaryWindow(r, "window", run.MaxSummaryWindow)
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read runs")
			return
		}
		hosts, err := store.FleetHealth(r.Context(), window)
		if err != nil {
			log.Error("server: fleet health: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute fleet health")
			return
		}
		// A host is shown when the caller can read at least one of the runs that put it here, and
		// only those run ids are listed. Otherwise the view reported work the same caller is
		// refused a 403 on by name.
		shown := make([]run.HostHealth, 0, len(hosts))
		for _, h := range hosts {
			visible := make([]string, 0, len(h.RecentRuns))
			for _, id := range h.RecentRuns {
				if keep(id) {
					visible = append(visible, id)
				}
			}
			if len(visible) == 0 && len(h.RecentRuns) > 0 {
				continue
			}
			h.RecentRuns = visible
			shown = append(shown, h)
		}
		respondJSON(w, log, http.StatusOK,
			fleetResponse{Hosts: shown, Count: len(shown), Window: window}, wantsPretty(r))
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
func driftHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: driftHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read runs")
			return
		}
		hosts, err := store.DriftStatus(r.Context())
		if err != nil {
			log.Error("server: drift status: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute drift status")
			return
		}
		// Each drift row names the check run that observed it, so it is kept only when that run is
		// readable, the same rule the run list applies. Deciding it once for the whole view instead
		// showed every host on the install, and their names and drifting task counts, to any caller
		// who could read a single run of their own.
		visible := make([]run.HostDrift, 0, len(hosts))
		for _, h := range hosts {
			if keep(h.RunID) {
				visible = append(visible, h)
			}
		}
		respondJSON(w, log, http.StatusOK,
			driftResponse{Hosts: visible, Count: len(visible)}, wantsPretty(r))
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
		if !decodeStrict(w, log, r.Body, &req) {
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

		// Authorize the check the way every other run operation authorizes a run, so a reconcile
		// cannot borrow a project, inventory, or credentials the actor was never granted, and a check
		// that names none of them is still scoped by the organization that ran it. Authorizing the
		// object list alone allowed the one case that matters most here: an inline playbook against an
		// inline inventory presents no objects, and authorizing no objects authorizes nothing, so any
		// operator on the install could turn another organization's drift check into a real change on
		// that organization's hosts.
		if denyOnAuthzError(w, log, authz.authorizeRun(r.Context(), grant.AccessUse, check)) {
			return
		}

		// The proposal reruns the check's own execution spec, so it carries the image, pull
		// credential, and timeout the check ran under. Hand-building the options dropped those, so a
		// reconcile of a containerized check escaped its pinned image and ran on the host under the
		// default timeout. The one difference from a plain rerun is the dry-run flag: the check
		// observed drift in check mode, the reconcile applies it for real, so DryRun is forced off
		// after the spec sets it.
		opts := append(check.ExecutionOptions(),
			run.WithDryRun(false),
			run.WithRequireApproval(true),
			run.WithProposedFrom(check.ID),
			run.WithSource("reconcile", check.ID),
			run.WithActor(actorName(r)), run.WithActorAccount(actorAccount(r)),
			run.WithActorType(actorType(r)),
		)
		if tool == run.ToolAnsible {
			// The Ansible fix reruns the playbook limited to the drifted host, applying exactly the
			// divergent tasks.
			opts = append(opts, run.WithLimit(host))
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
func hostHistoryHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: hostHistoryHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.PathValue("host")
		limit := summaryWindow(r, "limit", run.MaxHostHistory)
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read runs")
			return
		}
		history, err := store.HostHistory(r.Context(), host, limit)
		if err != nil {
			log.Error("server: host history: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read host history")
			return
		}
		visible := make([]run.HostSummary, 0, len(history))
		for _, h := range history {
			if keep(h.RunID) {
				visible = append(visible, h)
			}
		}
		respondJSON(w, log, http.StatusOK,
			hostHistoryResponse{Host: host, Runs: visible, Count: len(visible)}, wantsPretty(r))
	}
}

// taskTrendsHandler returns per task duration aggregates over recent runs.
func taskTrendsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: taskTrendsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		window := summaryWindow(r, "window", run.MaxSummaryWindow)
		_, anyReadable, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read runs")
			return
		}
		tasks, err := store.TaskTrends(r.Context(), window)
		if err != nil {
			log.Error("server: task trends: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute task trends")
			return
		}
		// Task names and their durations describe work, with no run id to check, so the view is
		// withheld from a caller who can read none of it.
		if !anyReadable {
			tasks = nil
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
func workersHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: workersHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		_, anyReadable, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read runs")
			return
		}
		workers, err := store.Workers(r.Context())
		if err != nil {
			log.Error("server: list workers: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list workers")
			return
		}
		// The executor list names who is running what, so it is withheld from a caller who may read
		// none of that work.
		if !anyReadable {
			workers = nil
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

// readyHandler reports whether the server can serve real work, not just that its process is up. It
// touches the store with a bounded query, so a database that is unreachable or still starting
// answers 503 and a load balancer holds traffic off until it responds. /healthz stays a pure
// liveness check that never touches the store, so the two probes mean different things: alive, and
// ready. A Kubernetes deployment wires /healthz to livenessProbe and /readyz to readinessProbe.
func readyHandler(store run.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, err := store.RunStatusCounts(ctx); err != nil {
			respondJSON(w, zap.NewNop(), http.StatusServiceUnavailable,
				map[string]any{"ready": false, "reason": "the run store is not reachable"}, false)
			return
		}
		respondJSON(w, zap.NewNop(), http.StatusOK, map[string]any{"ready": true}, false)
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

// parseFieldedQuery splits a search string into fielded terms and free text. status:, tool:,
// source:, actor:, and host: fill their filters, label:key=value matches a run label, and
// everything else stays free text. Explicit query parameters win over fielded terms.
func parseFieldedQuery(q string, filter *run.ListFilter) {
	var free []string
	for _, token := range strings.Fields(q) {
		key, value, ok := strings.Cut(token, ":")
		if !ok || value == "" {
			free = append(free, token)
			continue
		}
		switch strings.ToLower(key) {
		case "status":
			if filter.Status == "" {
				filter.Status = strings.ToLower(value)
			}
		case "tool":
			if filter.Tool == "" {
				filter.Tool = run.NormalizeTool(value)
			}
		case "source":
			filter.Source = strings.ToLower(value)
		case "actor":
			filter.Actor = value
		case "from":
			// The object that fired the run: a template or schedule id.
			filter.SourceID = value
		case "host":
			filter.Host = value
		case "label":
			if lk, lv, ok := strings.Cut(value, "="); ok && lk != "" {
				filter.LabelKey, filter.LabelValue = lk, lv
			} else {
				free = append(free, token)
			}
		default:
			free = append(free, token)
		}
	}
	filter.Query = strings.Join(free, " ")
}

// hostFactsHandler returns a host's most recently gathered system facts. A host that has never
// been through a fact-gathering play reports not found rather than an empty object, so the
// interface can say so plainly.
func hostFactsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read runs")
			return
		}
		facts, err := store.HostFactsFor(r.Context(), r.PathValue("host"))
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "no facts gathered for this host yet")
			return
		}
		if err != nil {
			log.Error("server: host facts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read host facts")
			return
		}
		// Facts are gathered by a run, and the run they came from is named on them, so a caller who
		// may not read that run may not read what it learned about the host either.
		if !keep(facts.RunID) {
			respondError(w, log, http.StatusNotFound, "no facts gathered for this host yet")
			return
		}
		respondJSON(w, log, http.StatusOK, facts, wantsPretty(r))
	}
}

// actorName returns the authenticated caller's audit name, empty when the API runs open.
func actorName(r *http.Request) string {
	if a, ok := actorFrom(r.Context()); ok {
		return a.Name
	}
	return ""
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

// actorAccount returns the account behind the caller's credential, empty when the API runs open or the
// credential names no account. It is stamped beside the actor's name because the name is the
// credential's and differs between a person's token and their browser session, so anything asking "is
// this the same person" has to compare the account.
func actorAccount(r *http.Request) string {
	if a, ok := actorFrom(r.Context()); ok {
		return a.UserID
	}
	return ""
}

// actorType returns how the caller authenticated, in the audit chain's vocabulary, empty when the
// API runs open. Stamped on submitted runs so a policy can tell an agent's request from a person's.
func actorType(r *http.Request) string {
	if a, ok := actorFrom(r.Context()); ok {
		return a.Type
	}
	return ""
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

// defaultRunsPage is the page size when a runs listing names none, and maxRunsPage is the largest
// page a caller can request, so one request can never materialize the whole run history.
const (
	defaultRunsPage = 200
	maxRunsPage     = 1000
)

// listRunsHandler returns a page of runs newest first, bounded even when no limit is given.
//
// The page is filtered to what the caller may read. Fetching one run already checked that, but the
// list did not, so under strict grants a caller who was refused a run by id could still read it, and
// everything on it, by listing. A run carries extra vars, a command line, and credential ids, so the
// list leaked more than the object it was listing.
func listRunsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: listRunsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		limit := queryInt(r, "limit")
		if limit <= 0 {
			// An explicit zero asks for everything, capped at the hard page bound so the promise
			// stays honest; an absent limit gets the smaller default.
			limit = defaultRunsPage
			if r.URL.Query().Get("limit") != "" {
				limit = maxRunsPage
			}
		}
		limit = min(limit, maxRunsPage)
		offset := queryInt(r, "offset")
		filter := run.ListFilter{
			Status:      r.URL.Query().Get("status"),
			OldestFirst: r.URL.Query().Get("order") == "oldest",
		}
		parseFieldedQuery(r.URL.Query().Get("q"), &filter)
		if tool := r.URL.Query().Get("tool"); tool != "" {
			filter.Tool = run.NormalizeTool(tool)
		}
		if after, err := time.Parse(time.RFC3339, r.URL.Query().Get("after")); err == nil {
			filter.After = after
		}
		if before, err := time.Parse(time.RFC3339, r.URL.Query().Get("before")); err == nil {
			filter.Before = before
		}
		runs, err := store.ListPage(r.Context(), filter, limit, offset)
		if err != nil {
			log.Error("server: list runs: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		// Whether another page follows is decided by what the store returned, before the read filter
		// thins it. Computing it from the trimmed page reported no more whenever the filter dropped a
		// row from a full page, so later readable runs never paged in.
		storeFullPage := len(runs) == limit
		runs, err = readableRuns(r.Context(), authz, runs)
		if err != nil {
			log.Error("server: filter runs: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		counts, err := store.RunStatusCounts(r.Context())
		if err != nil {
			log.Error("server: run status counts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		// The status totals are an install-wide aggregate, so they are withheld from a caller who may
		// read no runs at all, the same aggregate-withholding the drift and task views do. Otherwise
		// a strict-grants viewer refused every run by name still learned how much activity the install
		// had. A visible run on this page already proves the caller reads something, so the scan only
		// runs when the page is empty of readable runs.
		anyReadable := len(runs) > 0
		if !anyReadable {
			_, ar, ferr := derivedReadFilter(r.Context(), authz, store)
			if ferr != nil {
				log.Error("server: read filter: " + ferr.Error())
				respondError(w, log, http.StatusInternalServerError, "could not list runs")
				return
			}
			anyReadable = ar
		}
		summary := runSummary{}
		if anyReadable {
			summary = summarize(counts)
		}
		respondJSON(w, log, http.StatusOK, listRunsResponse{
			Runs:    maskRuns(runs),
			Count:   len(runs),
			Summary: summary,
			HasMore: storeFullPage,
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
		// Grade the run's blast radius so an approver sees the risk without opening the log.
		risk := run.AssessRisk(got)
		got.Risk = &risk
		respondJSON(w, log, http.StatusOK, maskRun(got), wantsPretty(r))
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
			shardsResponse{Shards: maskRuns(shards), Count: len(shards)}, wantsPretty(r))
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
			stepsResponse{Steps: maskRuns(steps), Count: len(steps)}, wantsPretty(r))
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
		var (
			after     int64
			atLineEnd = true
		)
		for {
			chunks, err := store.LogAfter(r.Context(), id, after, streamBatch)
			if err != nil {
				// The status line is already out, so a reader would otherwise receive a short log
				// that reads like the whole one. The log has a recorded digest to check a copy
				// against, which the event export does not, but the download should still say so
				// itself. The marker takes a line of its own so it is never read as part of
				// whatever the playbook was printing when the store went away.
				log.Error("server: get run log: " + err.Error())
				if !atLineEnd {
					if _, werr := w.Write([]byte("\n")); werr != nil {
						return
					}
				}
				writeExportSentinel(w, log, "the log store failed part way through this download")
				return
			}
			for _, c := range chunks {
				after = c.Seq
				if _, err := w.Write(c.Data); err != nil {
					log.Error("server: write run log: " + err.Error())
					return
				}
				if len(c.Data) > 0 {
					atLineEnd = c.Data[len(c.Data)-1] == '\n'
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

// exportSentinel is the trailing line a streaming export writes when it stops short. The status
// line left with the first byte of the body, so a truncated transfer still reads as a clean 200 and
// the file it produced still parses. The sentinel is the only thing that tells whoever opens the
// file that entries are missing from it.
type exportSentinel struct {
	// Incomplete is always true. Its presence in the file is the whole signal.
	Incomplete bool `json:"export_incomplete"`
	// Reason says in plain words what stopped the export.
	Reason string `json:"reason"`
}

// writeExportSentinel appends the incomplete marker as its own line to a body already in flight. A
// write that fails here means the reader is already gone, so there is nobody left to warn.
func writeExportSentinel(w http.ResponseWriter, log *zap.Logger, reason string) {
	line, err := json.Marshal(exportSentinel{Incomplete: true, Reason: reason})
	if err != nil {
		log.Error("server: marshal export sentinel: " + err.Error())
		return
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		log.Error("server: write export sentinel: " + err.Error())
	}
}

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
		// download=1 streams every event as a named NDJSON attachment, the export form tooling
		// consumes line by line; the paged JSON form below serves the UI.
		if r.URL.Query().Get("download") == "1" {
			// Paged rather than loaded whole. A run over a thousand hosts holds tens of thousands of
			// events, and reading them all before writing the first byte put the entire export in the
			// server's memory at once, for every concurrent download. The log export already streams;
			// this now matches it. NDJSON is written a line at a time, so a reader sees output
			// immediately and the server holds one page.
			var (
				enc     *json.Encoder
				flusher http.Flusher
				after   int64
				started bool
			)
			for {
				page, err := store.EventsAfter(r.Context(), id, after, maxEventsPage)
				if err != nil {
					log.Error("server: export run events: " + err.Error())
					if !started {
						// Nothing has been written, so this can still be an honest failure rather
						// than a file. A 500 with no attachment header cannot be mistaken for an
						// export the way a zero byte download can.
						respondError(w, log, http.StatusInternalServerError,
							"could not export run events")
						return
					}
					// The status line left with the first byte, so it cannot change now. A short
					// file that parses cleanly reads as a whole export, and this one is an audit
					// artifact, so its last line says it is incomplete.
					writeExportSentinel(w, log,
						"the event store failed part way through this export")
					return
				}
				if len(page) == 0 {
					return
				}
				if !started {
					started = true
					w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
					w.Header().Set("Content-Disposition",
						`attachment; filename="`+id+`-events.ndjson"`)
					enc = json.NewEncoder(w)
					flusher, _ = w.(http.Flusher)
				}
				for i := range page {
					if err := enc.Encode(&page[i]); err != nil {
						return
					}
				}
				after = page[len(page)-1].Seq
				if flusher != nil {
					flusher.Flush()
				}
				if len(page) < maxEventsPage {
					return
				}
			}
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

// streamKeepaliveTicks is how many quiet poll ticks pass between SSE comment lines. A run that
// prints nothing writes nothing, and an intermediary with a read timeout cuts an idle stream; a
// comment is invisible to EventSource and keeps the connection warm.
const streamKeepaliveTicks = 20

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
//
// A stream also ends when shutdown is canceled. Graceful shutdown waits for handlers to return and
// does not cancel a request's context, so without this a draining process would sit out its whole
// shutdown timeout for every stream still open. The stream closes without an end event, so the
// browser reconnects and resumes from its cursor once the process is back.
func runStreamHandler(streamer Streamer, store run.Store, authz *authorizer, log *zap.Logger,
	shutdown context.Context) http.HandlerFunc {
	if store == nil {
		panic("server: runStreamHandler: Store required")
	}
	// A nil channel blocks forever in a select, which is what an unset shutdown context should do.
	var draining <-chan struct{}
	if shutdown != nil {
		draining = shutdown.Done()
	}
	openStreams := &streamCount{}
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			respondError(w, log, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		// A stream is held open for as long as its run lasts, and each one keeps a goroutine, a
		// subscription, and a poll of the store every second. Nothing bounded how many one caller could
		// open, so a viewer, the lowest role that can read a run, could hold thousands open against
		// still-executing runs and drive the store at thousands of reads a second for as long as they
		// liked. The interface opens one per run page, so a real reader never approaches either limit.
		release, admitted := openStreams.admit(actorKeyFor(r))
		if !admitted {
			w.Header().Set("Retry-After", "5")
			respondError(w, log, http.StatusTooManyRequests,
				"too many live streams are open; close one before opening another")
			return
		}
		defer release()

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

		// A split or pipeline parent runs no tool of its own, so its own event log stays empty and
		// re-reading the store for it emits nothing. Its children publish their events under the
		// parent topic as they run, so the payload a wake carries is the only copy the parent stream
		// ever sees. Such a run forwards the wake payload rather than draining its own log.
		coordinator := rn.Kind == run.KindSplit || rn.Kind == run.KindPipeline

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
		quietTicks := 0
		for {
			select {
			case <-draining:
				return
			default:
			}
			if drain() {
				writeSSE(w, "end", streamCursor(lastSeq, logSeq), nil)
				flusher.Flush()
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-draining:
				return
			case msg, ok := <-wake:
				// A closed hub channel means the run ended and the topic is gone. Receiving
				// without checking made it always ready, so the loop stopped waiting and
				// re-queried the store as fast as it could: tens of thousands of statements a
				// second per connected stream. It fired exactly when the store was already
				// struggling, because that is when a run's stream is closed without the run
				// reaching a terminal state, so store trouble fed on itself.
				if !ok {
					// One last drain, so anything written between the previous read and the close
					// still reaches the browser, then end the stream the way a terminal run does.
					drain()
					writeSSE(w, "end", streamCursor(lastSeq, logSeq), nil)
					flusher.Flush()
					return
				}
				if coordinator && (msg.Type == "event" || msg.Type == "log") {
					// The parent's own log is empty, so the loop's drain has nothing to send for it.
					// Forward the child's event or log straight from the wake, which is the only
					// place a coordinator's live output exists. The cursor stays the parent's, which
					// carries no meaningful sequence, since the page merging shard histories folds
					// these by content and reconciles from the shards when the run ends.
					writeSSE(w, msg.Type, streamCursor(lastSeq, logSeq), msg.Data)
					flusher.Flush()
				}
			case <-ticker.C:
				quietTicks++
				if quietTicks >= streamKeepaliveTicks {
					quietTicks = 0
					if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
						return
					}
					flusher.Flush()
				}
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
