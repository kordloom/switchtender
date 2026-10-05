package server

import (
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/scrub"
)

// createScheduleRequest is the JSON body accepted by POST /schedules.
type createScheduleRequest struct {
	// Name identifies the schedule. Optional.
	Name string `json:"name"`
	// Cron is the cron expression that sets the cadence. Required unless RRule is set.
	Cron string `json:"cron"`
	// RRule is an RFC 5545 recurrence that sets the cadence instead of Cron, such as
	// "DTSTART;TZID=America/New_York:20260102T170000 RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR".
	RRule string `json:"rrule,omitempty"`
	// Timezone is the IANA name the cron expression is read in, such as America/New_York. Empty
	// leaves it in the server's local time, or for a recurrence, in the zone its DTSTART names.
	Timezone string `json:"timezone,omitempty"`
	// SpringForward says what happens to a time the clocks skip on the night they go forward: jump,
	// later, or skip, or empty for the default of the cadence. A pointer on the same rule as
	// Enabled: an update that omits it keeps the stored setting, so an edit dialog that does not
	// know the field cannot reset it, while an explicit empty string returns the schedule to its
	// default.
	SpringForward *string `json:"spring_forward,omitempty"`
	// Playbook is the playbook to run for a single or split schedule.
	Playbook string `json:"playbook"`
	// TemplateID fires a stored job template instead of the inline fields.
	TemplateID string `json:"template_id,omitempty"`
	// Inventory is the inventory to target.
	Inventory string `json:"inventory"`
	// Shards, when two or more, fires a split. A pointer so an update that omits it keeps the stored
	// shard count rather than collapsing a fleet-wide split onto one worker, while a caller that
	// means to flatten one sends zero explicitly.
	Shards *int `json:"shards,omitempty"`
	// Steps, when set, fires a pipeline of these steps. A pointer for the same reason as Shards: an
	// edit dialog that cannot express a graph must not erase one, while an empty array explicitly
	// replaces the pipeline with whatever the other fields name.
	Steps *[]run.PipelineStep `json:"steps,omitempty"`
	// Enabled reports whether the schedule fires. A pointer on the same rule as Shards and Steps: a
	// create that omits it means a live schedule, and an update that omits it keeps the schedule as it
	// is, so an edit dialog with no such control cannot pause or resume one by accident.
	//
	// Without this field the flag the tick loop honors could not be reached at all: pausing a nightly
	// deployment meant deleting it and losing its cadence and history, and an imported schedule that
	// arrived disabled could never be started.
	Enabled *bool `json:"enabled,omitempty"`
}

// scheduleEnabled resolves the fire flag for a write: what the request says, or what is stored when the
// request says nothing.
func scheduleEnabled(requested *bool, stored bool) bool {
	if requested == nil {
		return stored
	}
	return *requested
}

// scheduleSpringForward resolves the spring-forward setting for a write: what the request says, or
// what is stored when the request says nothing.
func scheduleSpringForward(requested *string, stored string) string {
	if requested == nil {
		return stored
	}
	return *requested
}

// scheduleShards resolves the shard count for a write: what the request says, or what is stored when
// the request says nothing.
func scheduleShards(requested *int, stored int) int {
	if requested == nil {
		return stored
	}
	return *requested
}

// scheduleSteps resolves the pipeline for a write, on the same rule as scheduleShards. An empty array
// is a real value and clears the pipeline; a missing field keeps it.
func scheduleSteps(requested *[]run.PipelineStep, stored []run.PipelineStep) []run.PipelineStep {
	if requested == nil {
		return stored
	}
	return *requested
}

// schedulesResponse wraps a schedule list.
type schedulesResponse struct {
	// Schedules is the list of schedules.
	Schedules []*schedule.Schedule `json:"schedules"`
	// Count is the number of schedules returned.
	Count int `json:"count"`
	// Total is how many rows exist before the response cap, so a caller shown a prefix knows it is
	// one. Equal to Count for every ordinary install.
	Total int `json:"total"`
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
		// A recurrence whose DTSTART names its zone carries that zone onto the schedule, so every
		// view of it says which zone it fires in rather than showing it as server time.
		zone := req.Timezone
		if zone == "" {
			zone = schedule.RecurrenceZone(req.RRule)
		}
		sc := &schedule.Schedule{
			ID: schedule.NewID(), Name: req.Name, Cron: req.Cron, RRule: req.RRule, Timezone: zone,
			Playbook:  req.Playbook,
			Inventory: req.Inventory, Shards: scheduleShards(req.Shards, 0),
			Steps:      scheduleSteps(req.Steps, nil),
			TemplateID: req.TemplateID, OrgID: run.SubmitterOrgFrom(r.Context()),
			Enabled: scheduleEnabled(req.Enabled, true), CreatedAt: time.Now(),
			// Recorded so an offboarding review can find what somebody set up. It does not change
			// when the schedule fires: deleting the person who created it deliberately does not stop
			// the automation, because halting production work the moment somebody leaves is its own
			// outage. What was missing was the record needed to make that call.
			CreatedBy: actorName(r),
			// Empty is the cadence's default: jump for a cron expression, later for a rule.
			SpringForward: scheduleSpringForward(req.SpringForward, ""),
		}
		// A schedule that names no zone is read in this server's zone, and that zone is written onto
		// it by name. Left unnamed, each server of a highly available pair read it in its own zone,
		// so a pair whose servers disagreed fired one daily schedule twice in a day.
		sc.PinZone(schedule.ServerZone())
		if err := sc.Validate(); err != nil {
			respondError(w, log, http.StatusBadRequest, scheduleInvalid(err))
			return
		}
		next, err := sc.NextFire(time.Now())
		if err != nil {
			// A bad zone and a bad expression are different mistakes, and reporting one as the
			// other sent operators to check a field that was correct.
			respondError(w, log, http.StatusBadRequest, scheduleTimeError(err))
			return
		}
		sc.NextRunAt = &next

		if err := store.Save(r.Context(), sc); err != nil {
			log.Error("server: save schedule: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not save schedule")
			return
		}
		w.Header().Set("Location", "/v1/schedules/"+sc.ID)
		respondSchedule(w, r, log, http.StatusCreated, sc)
	}
}

// updateScheduleHandler replaces a schedule, keeping its creation time and last-run record, and
// recomputes the next fire from the new cron. A request that names the enabled flag pauses or resumes
// the schedule; one that omits it leaves the flag as it stands.
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
		//
		// A recurrence whose DTSTART names a zone is read in that zone, so an edit that sends one
		// takes the zone from it rather than from what the schedule held before.
		zone := req.Timezone
		if zone == "" {
			zone = schedule.RecurrenceZone(req.RRule)
		}
		if zone == "" {
			zone = existing.Timezone
		}
		// A schedule's steps carry their own scripts, and the read scrubs them for anyone below
		// admin, so an ordinary edit submits the scrubbed form back. Storing that verbatim writes
		// the mask over the real command and the schedule fires the mask on its next tick.
		restoredSteps, rerr := scrub.Restore(stepsScrubber(r.Context()),
			scheduleSteps(req.Steps, existing.Steps), existing.Steps, "steps")
		if rerr != nil {
			respondError(w, log, http.StatusConflict, rerr.Error())
			return
		}
		sc := &schedule.Schedule{
			ID: id, Name: req.Name, Cron: req.Cron, RRule: req.RRule, Timezone: zone,
			Playbook:  req.Playbook,
			Inventory: req.Inventory, Shards: scheduleShards(req.Shards, existing.Shards),
			Steps:      restoredSteps,
			TemplateID: req.TemplateID, OrgID: existing.OrgID,
			Enabled: scheduleEnabled(req.Enabled, existing.Enabled), CreatedAt: existing.CreatedAt,
			LastRunAt: existing.LastRunAt, LastRunID: existing.LastRunID,
			// The skips are fires that already happened, which an edit does not undo. The editor
			// previews the inventory, so whoever edits sees whether the next fire will reach hosts.
			LastSkip: existing.LastSkip, SkippedFires: existing.SkippedFires,
			// Carried through an edit: the field records who set the schedule up, not who last
			// touched it, and rewriting it on every edit would erase exactly what it is for.
			CreatedBy: existing.CreatedBy,
			// Kept through an edit that does not name it, like the enabled flag.
			SpringForward: scheduleSpringForward(req.SpringForward, existing.SpringForward),
		}
		// A stored schedule that names no zone, which only an earlier release writes, is read in
		// schedule.UnnamedZone, so an edit that names none either writes that zone onto it rather
		// than moving when it fires.
		sc.PinZone(schedule.UnnamedZone)
		if err := sc.Validate(); err != nil {
			respondError(w, log, http.StatusBadRequest, scheduleInvalid(err))
			return
		}
		next, err := sc.NextFire(time.Now())
		if err != nil {
			// A bad zone and a bad expression are different mistakes, and reporting one as the
			// other sent operators to check a field that was correct.
			respondError(w, log, http.StatusBadRequest, scheduleTimeError(err))
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
		respondSchedule(w, r, log, http.StatusOK, sc)
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
		restricted, err := restrictedReader(r.Context(), authz)
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
		capped, total := cappedList(list)
		respondJSON(w, log, http.StatusOK,
			schedulesResponse{Schedules: scrubbedSchedules(r.Context(), capped), Count: len(capped),
				Total: total}, wantsPretty(r))
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
		respondSchedule(w, r, log, http.StatusOK, sc)
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
		err := deleteAttachableObject(r.Context(), store, id)
		if errors.Is(err, schedule.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "schedule not found")
			return
		}
		if respondCleanupChanged(w, log, err) {
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

// previewScheduleHandler returns the next five firings for a cron spec or an RFC 5545 recurrence,
// so a form can show what a schedule will do before saving it.
//
// The preview reads the same optional timezone a schedule carries. Without it the preview computed
// firings in the server's local zone while the saved schedule fired in its own, so a form promised
// times hours away from when the job actually ran.
//
// A recurrence bounded by COUNT or UNTIL may have fewer than five fires left. The ones it has are
// returned with finished set, so a form can say the rule stops rather than implying it repeats.
//
// The preview reads the spring-forward setting too, so the times it shows are the times the
// schedule fires. Five fires rarely reach the one night a year the setting matters, so when the
// schedule names a time the clocks skip within about thirteen months, spring_gap says which night,
// which readings the jump erases, and where the schedule fires that night under its setting.
func previewScheduleHandler(log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		spec, rule := q.Get("cron"), q.Get("rrule")
		if spec == "" && rule == "" {
			respondError(w, log, http.StatusBadRequest, "cron or rrule is required")
			return
		}
		zone := q.Get("timezone")
		if zone == "" {
			zone = schedule.RecurrenceZone(rule)
		}
		preview := &schedule.Schedule{Cron: spec, RRule: rule, Timezone: zone,
			SpringForward: q.Get("spring_forward")}
		// The preview reads a cadence that names no zone in the zone a create would pin to it, so the
		// times it shows are the times the saved schedule fires, and it answers that zone by name.
		preview.PinZone(schedule.ServerZone())
		zone = preview.Timezone
		if spec != "" && rule != "" {
			respondError(w, log, http.StatusBadRequest,
				"a schedule takes a cron expression or a recurrence rule, not both")
			return
		}
		next := make([]time.Time, 0, 5)
		after := time.Now()
		finished := false
		for range 5 {
			fire, err := preview.NextFire(after)
			if errors.Is(err, schedule.ErrExhausted) && len(next) > 0 {
				finished = true
				break
			}
			if err != nil {
				respondError(w, log, http.StatusBadRequest, scheduleTimeError(err))
				return
			}
			next = append(next, fire)
			after = fire
		}
		body := map[string]any{"next": next}
		if finished {
			body["finished"] = true
		}
		if zone != "" {
			body["timezone"] = zone
		}
		// The night is a courtesy beside the fires, which are the answer, so a schedule whose next
		// such night cannot be worked out still previews.
		if gap, gerr := preview.NextSpringGap(time.Now()); gerr == nil && gap != nil {
			body["spring_gap"] = gap
		}
		respondJSON(w, log, http.StatusOK, body, wantsPretty(r))
	}
}

// scheduleTimeError picks the message for a failure to compute a schedule's next firing. A bad
// timezone and a bad expression are different mistakes made in different fields, and reporting the
// first as the second sent an operator to check the one thing that was correct.
//
// A recurrence's own message names the part of the rule at fault, which is the only way to find it
// in a rule of several lines, so it is passed through as written.
func scheduleTimeError(err error) string {
	switch {
	case errors.Is(err, schedule.ErrBadTimezone), errors.Is(err, schedule.ErrBadRecurrence),
		errors.Is(err, schedule.ErrBadSpringForward):
		return err.Error()
	case errors.Is(err, schedule.ErrExhausted):
		return "the recurrence has no fire after now, so the schedule would never run"
	}
	return "invalid cron expression"
}

// scheduleInvalid picks the message for a schedule that does not validate.
func scheduleInvalid(err error) string {
	if errors.Is(err, schedule.ErrNoTarget) {
		return "a playbook, steps, or a template_id is required"
	}
	return scheduleTimeError(err)
}
