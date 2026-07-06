package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
)

// createScheduleRequest is the JSON body accepted by POST /schedules.
type createScheduleRequest struct {
	// Name identifies the schedule. Optional.
	Name string `json:"name"`
	// Cron is the cron expression that sets the cadence. Required.
	Cron string `json:"cron"`
	// Playbook is the playbook to run for a single or split schedule.
	Playbook string `json:"playbook"`
	// Inventory is the inventory to target.
	Inventory string `json:"inventory"`
	// Shards, when two or more, fires a split.
	Shards int `json:"shards"`
	// Steps, when set, fires a pipeline of these steps.
	Steps []run.PipelineStep `json:"steps"`
}

// schedulesResponse wraps a schedule list.
type schedulesResponse struct {
	// Schedules is the list of schedules.
	Schedules []*schedule.Schedule `json:"schedules"`
	// Count is the number of schedules returned.
	Count int `json:"count"`
}

// createScheduleHandler creates a recurring schedule.
func createScheduleHandler(store schedule.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "scheduling not enabled")
			return
		}
		var req createScheduleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}

		sc := &schedule.Schedule{
			ID: schedule.NewID(), Name: req.Name, Cron: req.Cron, Playbook: req.Playbook,
			Inventory: req.Inventory, Shards: req.Shards, Steps: req.Steps,
			Enabled: true, CreatedAt: time.Now(),
		}
		if err := sc.Validate(); err != nil {
			msg := "invalid schedule"
			switch {
			case errors.Is(err, schedule.ErrBadCron):
				msg = "invalid cron expression"
			case errors.Is(err, schedule.ErrNoTarget):
				msg = "a playbook or steps are required"
			}
			respondError(w, log, http.StatusBadRequest, msg)
			return
		}
		next, err := schedule.NextFire(sc.Cron, time.Now())
		if err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid cron expression")
			return
		}
		sc.NextRunAt = &next

		if err := store.Save(r.Context(), sc); err != nil {
			log.Error("server: save schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not save schedule")
			return
		}
		w.Header().Set("Location", "/schedules/"+sc.ID)
		respondJSON(w, log, http.StatusCreated, sc, wantsPretty(r))
	}
}

// listSchedulesHandler returns all schedules.
func listSchedulesHandler(store schedule.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "scheduling not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list schedules: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list schedules")
			return
		}
		respondJSON(w, log, http.StatusOK,
			schedulesResponse{Schedules: list, Count: len(list)}, wantsPretty(r))
	}
}

// getScheduleHandler returns a single schedule.
func getScheduleHandler(store schedule.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "scheduling not enabled")
			return
		}
		sc, err := store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, schedule.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "schedule not found")
			return
		}
		if err != nil {
			log.Error("server: get schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get schedule")
			return
		}
		respondJSON(w, log, http.StatusOK, sc, wantsPretty(r))
	}
}

// deleteScheduleHandler removes a schedule.
func deleteScheduleHandler(store schedule.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "scheduling not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, schedule.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "schedule not found")
			return
		}
		if err != nil {
			log.Error("server: delete schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete schedule")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"status": "deleted"}, wantsPretty(r))
	}
}
