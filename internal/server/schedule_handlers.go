package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// createScheduleRequest is the JSON body accepted by POST /schedules.
type createScheduleRequest struct {
	// Name identifies the schedule. Optional.
	Name string `json:"name"`
	// Cron is the cron expression that sets the cadence. Required.
	Cron string `json:"cron"`
	// Timezone is the IANA name the cron expression is read in, such as America/New_York. Empty
	// leaves it in the server's local time.
	Timezone string `json:"timezone,omitempty"`
	// Playbook is the playbook to run for a single or split schedule.
	Playbook string `json:"playbook"`
	// TemplateID fires a stored job template instead of the inline fields.
	TemplateID string `json:"template_id,omitempty"`
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
func createScheduleHandler(store schedule.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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

		// A schedule fires a template without anybody present, so writing one has to authorize the
		// template it will fire. Of the four ways to launch, run submission and template launch both
		// check, and a webhook trigger is covered when the trigger is written. A schedule checked at
		// no point in the chain, so it was the way to run a template a caller could not launch.
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, req.TemplateID)) {
			return
		}
		sc := &schedule.Schedule{
			ID: schedule.NewID(), Name: req.Name, Cron: req.Cron, Timezone: req.Timezone, Playbook: req.Playbook,
			Inventory: req.Inventory, Shards: req.Shards, Steps: req.Steps,
			TemplateID: req.TemplateID,
			Enabled:    true, CreatedAt: time.Now(),
		}
		if err := sc.Validate(); err != nil {
			msg := "invalid schedule"
			switch {
			case errors.Is(err, schedule.ErrBadCron):
				msg = "invalid cron expression"
			case errors.Is(err, schedule.ErrNoTarget):
				msg = "a playbook, steps, or a template_id is required"
			}
			respondError(w, log, http.StatusBadRequest, msg)
			return
		}
		next, err := sc.NextFire(time.Now())
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
		w.Header().Set("Location", "/v1/schedules/"+sc.ID)
		respondJSON(w, log, http.StatusCreated, sc, wantsPretty(r))
	}
}

// updateScheduleHandler replaces a schedule, keeping its enabled state, creation time, and last-run
// record, and recomputes the next fire from the new cron.
func updateScheduleHandler(store schedule.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		id := r.PathValue("id")
		existing, err := store.Get(r.Context(), id)
		if errors.Is(err, schedule.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "schedule not found")
			return
		}
		if err != nil {
			log.Error("server: read schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read schedule")
			return
		}
		// A schedule fires a template without anybody present, so writing one has to authorize the
		// template it will fire. Of the four ways to launch, run submission and template launch both
		// check, and a webhook trigger is covered when the trigger is written. A schedule checked at
		// no point in the chain, so it was the way to run a template a caller could not launch.
		// Both the template being named and the one already stored are authorized. Checking only the
		// body let a caller take over somebody else's schedule by leaving template_id out: nothing
		// was named, so nothing was checked, and the schedule was rewritten to run a playbook of
		// the caller's choosing on the original owner's timetable.
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, req.TemplateID, existing.TemplateID)) {
			return
		}
		sc := &schedule.Schedule{
			ID: id, Name: req.Name, Cron: req.Cron, Timezone: req.Timezone, Playbook: req.Playbook,
			Inventory: req.Inventory, Shards: req.Shards, Steps: req.Steps,
			TemplateID: req.TemplateID, Enabled: existing.Enabled, CreatedAt: existing.CreatedAt,
			LastRunAt: existing.LastRunAt, LastRunID: existing.LastRunID,
		}
		if err := sc.Validate(); err != nil {
			msg := "invalid schedule"
			switch {
			case errors.Is(err, schedule.ErrBadCron):
				msg = "invalid cron expression"
			case errors.Is(err, schedule.ErrNoTarget):
				msg = "a playbook, steps, or a template_id is required"
			}
			respondError(w, log, http.StatusBadRequest, msg)
			return
		}
		next, err := sc.NextFire(time.Now())
		if err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid cron expression")
			return
		}
		sc.NextRunAt = &next
		if err := store.Save(r.Context(), sc); err != nil {
			log.Error("server: update schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not save schedule")
			return
		}
		respondJSON(w, log, http.StatusOK, sc, wantsPretty(r))
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
func deleteScheduleHandler(store schedule.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "scheduling not enabled")
			return
		}
		id := r.PathValue("id")
		// Deleting a schedule silently stops work somebody relies on, so it asks the same question
		// writing one does: may this caller use the template behind it.
		existing, gerr := store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(gerr, schedule.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "schedule not found")
			return
		}
		if gerr != nil {
			log.Error("server: read schedule: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read schedule")
			return
		}
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, existing.TemplateID)) {
			return
		}
		err := store.Delete(r.Context(), id)
		if errors.Is(err, schedule.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "schedule not found")
			return
		}
		if err != nil {
			log.Error("server: delete schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete schedule")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": id}, wantsPretty(r))
	}
}

// previewScheduleHandler returns the next five firings for a cron spec, so a form can show what a
// schedule will do before saving it.
func previewScheduleHandler(log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		spec := r.URL.Query().Get("cron")
		if spec == "" {
			respondError(w, log, http.StatusBadRequest, "cron is required")
			return
		}
		next := make([]time.Time, 0, 5)
		after := time.Now()
		for range 5 {
			fire, err := schedule.NextFire(spec, after)
			if err != nil {
				respondError(w, log, http.StatusBadRequest, "invalid cron expression")
				return
			}
			next = append(next, fire)
			after = fire
		}
		respondJSON(w, log, http.StatusOK, map[string]any{"next": next}, wantsPretty(r))
	}
}
