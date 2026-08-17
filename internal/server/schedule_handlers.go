package server

import (
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
		if !decodeStrict(w, log, r.Body, &req) {
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
		// The creating actor's organization is stamped on the schedule, which is what scopes an
		// inline one: it names no template, so there is no grantable object to scope it by and
		// without an owner it belongs to everybody. The org is the one the request already carries,
		// resolved once beside the actor, so a schedule and a run submitted by the same caller are
		// stamped with the same tenant.
		sc := &schedule.Schedule{
			ID: schedule.NewID(), Name: req.Name, Cron: req.Cron, Timezone: req.Timezone, Playbook: req.Playbook,
			Inventory: req.Inventory, Shards: req.Shards, Steps: req.Steps,
			TemplateID: req.TemplateID, OrgID: run.SubmitterOrgFrom(r.Context()),
			Enabled: true, CreatedAt: time.Now(),
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
		if !decodeStrict(w, log, r.Body, &req) {
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
		// the caller's choosing on the original owner's timetable. The stored schedule is asked the
		// full question, so an inline one, which names no template at all, is scoped by its owning
		// organization rather than authorized by default over zero objects.
		if denyOnAuthzError(w, log, authz.authorizeSchedule(r.Context(), grant.AccessUse, existing)) {
			return
		}
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, req.TemplateID)) {
			return
		}
		// The owning organization is the schedule's, not the editor's, so an edit cannot move a
		// schedule into the editor's tenant or strand it as unowned.
		// A cron expression means nothing without the zone it is read in, and no edit dialog in the
		// product renders one, so an edit that named no zone silently moved when the schedule fires:
		// an imported schedule pinned to America/New_York began firing in the server's local time,
		// hours off, with nothing on screen to show it. An empty zone from an editor means "leave it
		// as it is"; a caller that wants server-local time can send it explicitly as UTC.
		zone := req.Timezone
		if zone == "" {
			zone = existing.Timezone
		}
		sc := &schedule.Schedule{
			ID: id, Name: req.Name, Cron: req.Cron, Timezone: zone, Playbook: req.Playbook,
			Inventory: req.Inventory, Shards: req.Shards, Steps: req.Steps,
			TemplateID: req.TemplateID, OrgID: existing.OrgID,
			Enabled: existing.Enabled, CreatedAt: existing.CreatedAt,
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
		// A delete landing between the read above and this write must win. Save is an upsert, so it
		// would have re-created the schedule the operator just removed and left it firing.
		if err := store.Update(r.Context(), sc); errors.Is(err, schedule.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "schedule not found")
			return
		} else if err != nil {
			log.Error("server: update schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not save schedule")
			return
		}
		respondJSON(w, log, http.StatusOK, sc, wantsPretty(r))
	}
}

// listSchedulesHandler returns the schedules whose template the caller may use.
//
// Reading was unauthorized while writing and deleting were not, so any operator could enumerate the
// whole estate's unattended automation: which template fires on what cron, against which inventory.
// A schedule is visible on the same test that governs writing one, its template, so listing and
// editing cannot disagree about who it belongs to.
func listSchedulesHandler(store schedule.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		restricted, err := grantsEnforced(r.Context(), authz)
		if err != nil {
			log.Error("server: read filter: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list schedules")
			return
		}
		if restricted {
			kept := make([]*schedule.Schedule, 0, len(list))
			for _, sc := range list {
				if authz.authorizeSchedule(r.Context(), grant.AccessUse, sc) == nil {
					kept = append(kept, sc)
				}
			}
			list = kept
		}
		respondJSON(w, log, http.StatusOK,
			schedulesResponse{Schedules: list, Count: len(list)}, wantsPretty(r))
	}
}

// getScheduleHandler returns a single schedule the caller may see.
//
// Without the check this was a direct object reference: any id returned the schedule, its cron, its
// inventory, and the template it fires.
func getScheduleHandler(store schedule.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		if denyOnAuthzError(w, log, authz.authorizeSchedule(r.Context(), grant.AccessUse, sc)) {
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
		// reading and writing one do: the template behind it when it fires one, and the owning
		// organization when it is inline and there is no template to ask about.
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
		if denyOnAuthzError(w, log, authz.authorizeSchedule(r.Context(), grant.AccessUse, existing)) {
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
//
// The preview reads the same optional timezone a schedule carries. Without it the preview computed
// firings in the server's local zone while the saved schedule fired in its own, so a form promised
// times hours away from when the job actually ran.
func previewScheduleHandler(log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		spec := r.URL.Query().Get("cron")
		if spec == "" {
			respondError(w, log, http.StatusBadRequest, "cron is required")
			return
		}
		preview := &schedule.Schedule{Cron: spec, Timezone: r.URL.Query().Get("timezone")}
		next := make([]time.Time, 0, 5)
		after := time.Now()
		for range 5 {
			fire, err := preview.NextFire(after)
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
