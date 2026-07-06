package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/run"
)

// defaultFleetWindow is the number of recent runs per host considered when no window is given.
const defaultFleetWindow = 10

// createRunRequest is the JSON body accepted by POST /runs.
type createRunRequest struct {
	// Playbook is the path to the playbook to execute. Required.
	Playbook string `json:"playbook"`
	// Inventory is the path to the inventory to target. Optional.
	Inventory string `json:"inventory"`
	// Shards, when two or more, splits the run across that many inventory slices.
	Shards int `json:"shards,omitempty"`
}

// createPipelineRequest is the JSON body accepted by POST /pipelines.
type createPipelineRequest struct {
	// Name identifies the pipeline. Optional.
	Name string `json:"name"`
	// Inventory is the default inventory for steps that do not set their own. Optional.
	Inventory string `json:"inventory"`
	// Steps is the ordered list of steps to run. Required, at least one.
	Steps []run.PipelineStep `json:"steps"`
}

// listRunsResponse wraps a run list. The envelope leaves room for pagination fields later.
type listRunsResponse struct {
	// Runs is the ordered list of runs.
	Runs []*run.Run `json:"runs"`
	// Count is the number of runs returned.
	Count int `json:"count"`
}

// eventsResponse wraps a run's structured events.
type eventsResponse struct {
	// Events is the ordered list of events.
	Events []event.Event `json:"events"`
	// Count is the number of events returned.
	Count int `json:"count"`
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

// healthHandler reports service liveness.
func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, zap.NewNop(), http.StatusOK, map[string]string{"status": "ok"}, false)
	}
}

// createRunHandler accepts a run request and submits it for execution.
func createRunHandler(submitter Submitter, log *zap.Logger) http.HandlerFunc {
	if submitter == nil {
		panic("server: createRunHandler: Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req createRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Playbook == "" {
			respondError(w, log, http.StatusBadRequest, "playbook is required")
			return
		}

		var created *run.Run
		var err error
		if req.Shards >= 2 {
			created, err = submitter.SubmitSplit(r.Context(), req.Playbook, req.Inventory, req.Shards)
		} else {
			created, err = submitter.Submit(r.Context(), req.Playbook, req.Inventory)
		}
		if err != nil {
			log.Error("server: submit run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not submit run")
			return
		}

		w.Header().Set("Location", "/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
	}
}

// createPipelineHandler accepts a pipeline request and submits it for execution.
func createPipelineHandler(submitter Submitter, log *zap.Logger) http.HandlerFunc {
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
			if step.Playbook == "" {
				respondError(w, log, http.StatusBadRequest, "each step requires a playbook")
				return
			}
		}

		created, err := submitter.SubmitPipeline(r.Context(), req.Name, req.Inventory, req.Steps)
		switch {
		case errors.Is(err, dispatch.ErrUnnamedStep), errors.Is(err, dispatch.ErrDuplicateStep),
			errors.Is(err, dispatch.ErrUnknownDependency), errors.Is(err, dispatch.ErrDependencyCycle):
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			log.Error("server: submit pipeline: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not submit pipeline")
			return
		}
		w.Header().Set("Location", "/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
	}
}

// cancelRunHandler stops a pending or executing run.
func cancelRunHandler(store run.Store, canceler Canceler, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: cancelRunHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if canceler == nil {
			respondError(w, log, http.StatusNotFound, "cancellation not enabled")
			return
		}
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
		if existing.Status.Terminal() {
			respondError(w, log, http.StatusConflict, "run already finished")
			return
		}
		if !canceler.Cancel(id) {
			respondError(w, log, http.StatusConflict, "run is not cancelable")
			return
		}
		respondJSON(w, log, http.StatusAccepted,
			map[string]string{"status": "canceling"}, wantsPretty(r))
	}
}

// retryRunHandler starts a new split run from the failed shards of a finished one.
func retryRunHandler(retrier Retrier, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if retrier == nil {
			respondError(w, log, http.StatusNotFound, "retry not enabled")
			return
		}
		created, err := retrier.RetryFailedShards(r.Context(), r.PathValue("id"))
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
		w.Header().Set("Location", "/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
	}
}

// listRunsHandler returns all runs newest first.
func listRunsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: listRunsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		runs, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list runs: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		respondJSON(w, log, http.StatusOK, listRunsResponse{Runs: runs, Count: len(runs)}, wantsPretty(r))
	}
}

// getRunHandler returns a single run by id.
func getRunHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
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
		respondJSON(w, log, http.StatusOK, got, wantsPretty(r))
	}
}

// runShardsHandler returns the shard runs of a parent run.
func runShardsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runShardsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := store.Get(r.Context(), id); errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
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
func runStepsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runStepsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := store.Get(r.Context(), id); errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
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
func runLogsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runLogsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := store.Log(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			log.Error("server: get run log: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run log")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			log.Error("server: write run log: " + err.Error())
		}
	}
}

// runEventsHandler returns a run's structured events as JSON.
func runEventsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runEventsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		events, err := store.Events(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			log.Error("server: get run events: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run events")
			return
		}
		respondJSON(w, log, http.StatusOK,
			eventsResponse{Events: events, Count: len(events)}, wantsPretty(r))
	}
}

// runStreamHandler streams a run's live events and log over Server Sent Events. The stream ends when
// the run finishes or, for a run already terminal in the store, immediately.
func runStreamHandler(streamer Streamer, store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runStreamHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if streamer == nil {
			respondError(w, log, http.StatusNotFound, "streaming not enabled")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			respondError(w, log, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		id := r.PathValue("id")
		if _, err := store.Get(r.Context(), id); errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}

		ch, cancel := streamer.Subscribe(id)
		defer cancel()

		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Re-read after subscribing so a run that finished around now still ends the stream.
		if rn, err := store.Get(r.Context(), id); err == nil && rn.Status.Terminal() {
			writeSSE(w, "end", nil)
			flusher.Flush()
			return
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				writeSSE(w, msg.Type, msg.Data)
				flusher.Flush()
				if msg.Type == "end" {
					return
				}
			}
		}
	}
}

// writeSSE writes one Server Sent Event with the given event name and JSON data. A write failure
// means the client went away; the stream loop ends on the closed request context.
func writeSSE(w http.ResponseWriter, name string, data []byte) {
	if len(data) == 0 {
		data = []byte("null")
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}
