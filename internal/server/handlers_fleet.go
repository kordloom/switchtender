package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/license"
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

// fleetResponse wraps the fleet health ranking.
type fleetResponse struct {
	// Hosts is the ranking of hosts by recent failures, worst first.
	Hosts []run.HostHealth `json:"hosts"`
	// Count is the number of hosts returned.
	Count int `json:"count"`
	// Total is how many hosts the caller may read, before the cap below was applied, so a reader
	// shown the worst thousand of ten thousand is told which thousand they are looking at.
	Total int `json:"total"`
	// Window is the number of recent runs per host considered.
	Window int `json:"window"`
}

// maxFleetHosts bounds how many host rows one fleet or drift response carries.
//
// window bounds the runs summarized PER HOST and was mistaken for a bound on the response: neither
// endpoint capped the hosts, so a ten thousand host estate serialized ten thousand records on every
// front-page load, and raising window multiplied the work behind each one. Both views are ranked
// worst first, so a bounded prefix is the part anybody reads; the rest is weight.
const maxFleetHosts = 1000

// cappedHosts returns at most maxFleetHosts of a worst-first ranking, and the full length, so the
// response can say what it left out.
func cappedHosts[T any](all []T) (shown []T, total int) {
	if len(all) <= maxFleetHosts {
		return all, len(all)
	}
	return all[:maxFleetHosts], len(all)
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
	// Withheld reports that the install-wide aggregate was not served because this caller's reads
	// are grant-scoped and these rows carry no run ids to scope by. Absent it, an empty list is
	// indistinguishable from an install that has simply run nothing.
	Withheld bool `json:"withheld,omitempty"`
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
			kept, ok := filterHostHealth(h, keep)
			if !ok {
				continue
			}
			shown = append(shown, kept)
		}
		capped, total := cappedHosts(shown)
		respondJSON(w, log, http.StatusOK,
			fleetResponse{Hosts: capped, Count: len(capped), Total: total, Window: window},
			wantsPretty(r))
	}
}

// driftResponse wraps the fleet drift status.
type driftResponse struct {
	// Hosts is each host's latest drift check, worst drift first.
	Hosts []run.HostDrift `json:"hosts"`
	// Count is the number of hosts returned.
	Count int `json:"count"`
	// Total is how many drifted hosts the caller may read, before the cap.
	Total int `json:"total"`
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
		capped, total := cappedHosts(visible)
		respondJSON(w, log, http.StatusOK,
			driftResponse{Hosts: capped, Count: len(capped), Total: total}, wantsPretty(r))
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
		// Seeing drift is Community; the one-click reconcile proposal is Team. The gate sits
		// before the body is even read, so a refusal does no work and changes nothing.
		if aerr := license.Allow(license.FeatureReconcile); aerr != nil {
			respondError(w, log, http.StatusForbidden, aerr.Error())
			return
		}
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
		// A reconcile turns an observation into a real change, so it is authorized as the
		// re-execution it is, through the one function that decides what that requires. Spelling
		// the parts out here is what let this path authorize the check's objects and not its queue.
		if authorizeReexecute(w, r, authz, log, check) {
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
		switch {
		// A refusal is the operator's answer, not the server's failure. Both of these were logged
		// and returned as a 500 saying the proposal could not be created, so the one person who
		// needed to know which rule stopped them had to be handed a server log to find out.
		case errors.Is(err, dispatch.ErrPolicyDenied) ||
			errors.Is(err, dispatch.ErrQueueUnlicensed):
			respondError(w, log, http.StatusForbidden, err.Error())
			return
		case err != nil:
			log.Error("server: reconcile submit: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create the proposal")
			return
		}
		respondRun(w, r, log, http.StatusAccepted, proposal)
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
		// This aggregate is install-wide and carries no run ids to filter, so it is only safe to
		// serve whole to a caller who may read everything. A grant-scoped caller used to receive
		// it anyway: task names, counts, and durations from every other tenant's runs, on the
		// strength of being able to read one run of their own. Withheld beats leaked, and the
		// response says which it is doing rather than serving an empty list that reads as a quiet
		// install.
		scoped, serr := restrictedReader(r.Context(), authz)
		if serr != nil {
			log.Error("server: read filter: " + serr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read runs")
			return
		}
		withheld := false
		if scoped || !anyReadable {
			tasks, withheld = nil, scoped
		}
		respondJSON(w, log, http.StatusOK,
			taskTrendsResponse{Tasks: tasks, Count: len(tasks), Window: window,
				Withheld: withheld}, wantsPretty(r))
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

// filterHostHealth narrows one host's health to the runs keep admits, and reports whether the host
// still has anything to show.
//
// Recent and RecentRuns are index-parallel by contract: entry i is the outcome of run i, and the host
// page draws its sparkline by walking them together so every tick links to the run that produced it.
// Compacting the ids and leaving the outcomes whole broke that pairing, so the ticks drew the full
// outcome history against whichever ids survived and a tick labeled failed linked to a run that
// succeeded, on the page whose entire job is pointing at the run that broke. The counts were worse:
// failures, total, the flip count, and the last outcome all kept describing runs the caller is
// refused by name, which is the invariant the handler's own comment claims to hold. Everything
// derived from the pair is recomputed here over what survived, so the two cannot drift apart again.
func filterHostHealth(h run.HostHealth, keep func(string) bool) (run.HostHealth, bool) {
	visible := make([]string, 0, len(h.RecentRuns))
	outcomes := make([]string, 0, len(h.RecentRuns))
	failures := 0
	for i, id := range h.RecentRuns {
		if !keep(id) {
			continue
		}
		outcome := ""
		if i < len(h.Recent) {
			outcome = h.Recent[i]
		}
		visible = append(visible, id)
		outcomes = append(outcomes, outcome)
		if run.FailedOutcome(outcome) {
			failures++
		}
	}
	// A host whose runs are all hidden is not shown at all. One that never carried run ids, which is
	// history recorded before they were tracked, is left as it stands rather than emptied.
	if len(visible) == 0 && len(h.RecentRuns) > 0 {
		return run.HostHealth{}, false
	}
	if len(h.RecentRuns) == 0 {
		return h, true
	}
	h.RecentRuns, h.Recent = visible, outcomes
	h.Failures, h.Total = failures, len(visible)
	h.Flips = run.FlipOutcomes(outcomes)
	h.Flaky = h.Flips >= 2
	if len(outcomes) > 0 {
		h.LastOutcome = outcomes[0]
	}
	return h, true
}

// estateResponse carries the estate as it stood at an instant.
type estateResponse struct {
	// At is the instant asked about, echoed so a stored answer says what question it answers.
	At time.Time `json:"at"`
	// Hosts are the facts in effect at that instant, ordered by host.
	Hosts []run.HostFacts `json:"hosts"`
	// Total is how many hosts were in effect before the response was capped.
	Total int `json:"total"`
	// Truncated reports that Hosts holds fewer than Total, so a caller does not read a prefix as
	// the whole estate.
	Truncated bool `json:"truncated,omitempty"`
	// Horizon is the oldest retained reading, and BeforeHistory reports that the instant asked
	// about predates it. Without them an empty estate is ambiguous: it looks the same whether the
	// fleet did not exist yet or the records simply do not reach that far, and an auditor reading
	// the wrong one concludes something false about the estate.
	Horizon *time.Time `json:"horizon,omitempty"`
	// BeforeHistory reports that the instant asked about is older than any retained reading.
	BeforeHistory bool `json:"before_history,omitempty"`
	// Withheld counts hosts left out because the run that gathered them could not be read, which
	// includes the case where that run has been deleted by retention. It is reported rather than
	// left silent: facts outlive the runs that gathered them on purpose, so asking about a date far
	// enough back reaches readings whose run is gone, and a grant-restricted caller would otherwise
	// be handed an empty estate with nothing to say why.
	Withheld int `json:"withheld,omitempty"`
}

// estateHandler answers what the estate looked like at an instant.
//
// This is the question a live view cannot answer, because a live view holds only what is true now.
// An auditor asks what was running on the date of the last review, and until the state history
// existed the honest answer was that nobody could say: each gather overwrote the one before it.
//
// The ?at= parameter is RFC 3339 and defaults to now, which makes the current estate the zero
// argument case of the same query rather than a separate endpoint.
func estateHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: estateHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		at, ok := instantParam(w, log, r, "at", time.Now())
		if !ok {
			return
		}
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the estate")
			return
		}
		hosts, err := store.EstateAt(r.Context(), at, maxListRows+1)
		if err != nil {
			log.Error("server: estate at: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the estate")
			return
		}
		// Facts carry the run that gathered them, so a caller who may not read that run may not
		// read what it learned about the host. Filtered per row rather than on the whole answer,
		// the way one host's facts already are: an estate view that skipped this would be the
		// widest read in the product and the easiest way around every grant on it.
		visible := make([]run.HostFacts, 0, len(hosts))
		withheld := 0
		for _, f := range hosts {
			if keep(f.RunID) {
				visible = append(visible, f)
				continue
			}
			withheld++
		}
		shown, total := cappedList(visible)
		resp := estateResponse{
			At: at, Hosts: shown, Total: total, Truncated: len(shown) < total, Withheld: withheld,
		}
		// The horizon is reported whenever it is known, and an instant before it is called out. A
		// failure to read it leaves both absent rather than failing the request: the estate is the
		// answer, and this only says how far back the answer can be trusted.
		if horizon, herr := store.EstateHorizon(r.Context()); herr == nil && !horizon.IsZero() {
			resp.Horizon = &horizon
			resp.BeforeHistory = at.Before(horizon)
		}
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}
