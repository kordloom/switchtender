// Package schedule holds recurring run schedules and the logic that fires them on a cron cadence.
// A schedule fires a single run, a split, or a pipeline through the same submit paths a client uses,
// so scheduled work is indistinguishable from manual work once it lands.
package schedule

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
)

// NewID returns a random schedule identifier prefixed with "sch_".
func NewID() string {
	return idgen.New("sch_", 8)
}

var (
	// ErrBadCron is returned when a cron expression cannot be parsed.
	ErrBadCron = errors.New("bad cron")
	// ErrNotFound is returned when a schedule does not exist.
	ErrNotFound = errors.New("schedule not found")
	// ErrNoTarget is returned when a schedule names neither a playbook nor pipeline steps.
	ErrNoTarget = errors.New("no playbook or steps")
)

// Schedule is a recurring run definition. It fires a pipeline when Steps is set, a split when Shards
// is two or more, and otherwise a single run.
type Schedule struct {
	// ID is the unique schedule identifier.
	ID string `json:"id"`
	// Name identifies the schedule.
	Name string `json:"name"`
	// Cron is the cron expression that sets the cadence.
	Cron string `json:"cron"`
	// Timezone is the IANA name, such as America/New_York, the cron expression is read in. Empty
	// means the server's local time, so a schedule made before this field behaves as before. A
	// named zone makes "0 9 * * 1" mean nine in that zone through daylight saving changes, not nine
	// wherever the server happens to sit.
	Timezone string `json:"timezone,omitempty"`
	// Playbook is the playbook to run for a single or split schedule.
	Playbook string `json:"playbook,omitempty"`
	// Inventory is the inventory to target.
	Inventory string `json:"inventory,omitempty"`
	// Shards, when two or more, fires a split across that many inventory slices.
	Shards int `json:"shards,omitempty"`
	// Steps, when set, fires a pipeline of these steps.
	Steps []run.PipelineStep `json:"steps,omitempty"`
	// TemplateID, when set, fires a stored job template instead of the inline fields.
	TemplateID string `json:"template_id,omitempty"`
	// OrgID is the owning organization stamped from the creating actor. It is what scopes a
	// schedule that names no template: an inline schedule carries a playbook or a shell command
	// line and no grantable object, so there is nothing for the per-object grant check to filter on
	// and the schedule would otherwise be readable, editable, and deletable across every tenant. A
	// crontab import produces these by the hundred, each holding a full command line. Empty for a
	// schedule created outside an actor's request, such as an import or a seeded demo, which under
	// strict grants leaves it visible to admins alone.
	OrgID string `json:"org_id,omitempty"`
	// Enabled reports whether the schedule fires.
	Enabled bool `json:"enabled"`
	// CreatedAt is when the schedule was created.
	CreatedAt time.Time `json:"created_at"`
	// NextRunAt is when the schedule fires next.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	// LastRunAt is when the schedule last fired.
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// LastRunID is the run created by the most recent fire.
	LastRunID string `json:"last_run_id,omitempty"`
}

// Clone returns a deep copy so callers cannot mutate stored state through shared pointers.
func (s *Schedule) Clone() *Schedule {
	if s == nil {
		return nil
	}
	out := *s
	if s.NextRunAt != nil {
		t := *s.NextRunAt
		out.NextRunAt = &t
	}
	if s.LastRunAt != nil {
		t := *s.LastRunAt
		out.LastRunAt = &t
	}
	if len(s.Steps) > 0 {
		out.Steps = make([]run.PipelineStep, len(s.Steps))
		copy(out.Steps, s.Steps)
	}
	return &out
}

// Validate reports whether the schedule has a parseable cron, a valid timezone, and a target to run.
func (s *Schedule) Validate() error {
	if _, err := s.NextFire(time.Now()); err != nil {
		return err
	}
	if s.Playbook == "" && len(s.Steps) == 0 && s.TemplateID == "" {
		return ErrNoTarget
	}
	return nil
}

// effectiveCron returns the cron expression with the schedule's timezone applied, so the same
// expression fires in the schedule's zone rather than the server's. An unset timezone leaves the
// expression as the caller wrote it, keeping the server's local time.
func (s *Schedule) effectiveCron() string {
	if s.Timezone == "" {
		return s.Cron
	}
	return "CRON_TZ=" + s.Timezone + " " + s.Cron
}

// NextFire returns the next time this schedule fires after the given time, in its own timezone. A
// bad timezone is reported here rather than stored, since the cron parser rejects an unknown zone.
func (s *Schedule) NextFire(after time.Time) (time.Time, error) {
	return NextFire(s.effectiveCron(), after)
}

// NextFire returns the next time the cron expression fires after the given time.
//
// A parseable expression that can never come due is refused here rather than passed on. The cron
// library gives up after scanning five years and returns the zero time with no error, and the
// scheduler reads any next-run time that is not after now as due. So "0 0 30 2 *", a February the
// thirtieth that will never exist, was stored, read as due on every tick, claimed, fired, and
// rewritten to zero again: one authenticated call produced a run every fifteen seconds forever,
// with nothing logged and no rate limit in front of it.
func NextFire(spec string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrBadCron, err)
	}
	next := sched.Next(after)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: %q parses but never comes due, so it would be read as "+
			"due on every tick", ErrBadCron, spec)
	}
	return next, nil
}
