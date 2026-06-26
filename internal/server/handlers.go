package server

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// Shards, when two or more, splits the run across that many inventory slices.
	Shards int `json:"shards,omitempty"`
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

// writeSSE writes one Server Sent Event with the given event name and JSON data.
func writeSSE(w http.ResponseWriter, name string, data []byte) {
	if len(data) == 0 {
		data = []byte("null")
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}
