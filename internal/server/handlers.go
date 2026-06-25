package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/run"
)

// createRunRequest is the JSON body accepted by POST /runs.
type createRunRequest struct {
	// Playbook is the path to the playbook to execute. Required.
	Playbook string `json:"playbook"`
	// Inventory is the path to the inventory to target. Optional.
	Inventory string `json:"inventory"`
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

		created, err := submitter.Submit(r.Context(), req.Playbook, req.Inventory)
		if err != nil {
			log.Error("server: submit run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not submit run")
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
