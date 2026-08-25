// Package schedule holds recurring run schedules and the logic that fires them on a cron cadence.
// A schedule fires a single run, a split, or a pipeline through the same submit paths a client uses,
// so scheduled work is indistinguishable from manual work once it lands.
package schedule

import (
	"errors"
	"fmt"
	"strings"
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
	if err := s.validTimezone(); err != nil {
		return err
	}
	if _, err := s.NextFire(time.Now()); err != nil {
		return err
	}
	if s.Playbook == "" && len(s.Steps) == 0 && s.TemplateID == "" {
		return ErrNoTarget
	}
	return nil
}

// validTimezone reports whether the timezone is a bare zone name this system can resolve.
//
// The zone is spliced in front of the cron expression as a CRON_TZ descriptor, and the parser splits that
// descriptor at the first space, so anything after a zone name becomes cron fields. Unchecked, a schedule
// could be stored with a blank cron and a timezone of "UTC * * * * *": it validated, computed a next fire
// a minute out, and fired every minute, while every view of it showed no cadence at all. A schedule whose
// displayed cadence is not the one it runs is the opposite of what recording unattended work is for.
//
// An unresolvable zone is refused here rather than at fire time, so it is reported to whoever wrote it
// instead of quietly running in the server's own time.
func (s *Schedule) validTimezone() error {
	if s.Timezone == "" {
		return nil
	}
	if strings.ContainsAny(s.Timezone, " \t\r\n=") {
		return fmt.Errorf("%w: a timezone is a zone name such as America/New_York, not %q",
			ErrBadCron, s.Timezone)
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("%w: timezone %q cannot be resolved on this system", ErrBadCron, s.Timezone)
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
	// On the day a zone falls back, the same local minute comes round twice, an hour apart, and the
	// cron library returns both. The scheduler advances from the moment it fired, so a nightly job
	// inside the repeated hour fired, advanced to the same wall-clock minute in the new offset, and
	// fired again: two full executions of the same non-idempotent playbook on one nominal day. A
	// cron slot is minute-granular, so two distinct instants sharing a local minute can only be that
	// repeat, and the second one is skipped.
	if sameLocalMinute(next, after) {
		next = sched.Next(next)
		if next.IsZero() {
			return time.Time{}, fmt.Errorf("%w: %q parses but never comes due after the "+
				"daylight-saving repeat", ErrBadCron, spec)
		}
	}
	// The other transition loses a fire rather than doubling one. On the day a zone springs forward
	// the scheduled wall clock may not exist at all, and the cron library steps over the whole day to
	// the next one, so a nightly job set for the skipped hour simply does not run that day, silently
	// and once a year. Firing at the instant the clock jumped is the closest real time to what was
	// asked for, and it is what a person who wrote "run at 02:00 nightly" means on the one night
	// 02:00 is not a time.
	if skipped, ok := skippedBySpringForward(spec, after, next); ok {
		return skipped, nil
	}
	return next, nil
}

// skippedBySpringForward reports the instant to fire at when the zone jumped over the scheduled wall
// clock between after and next, and whether that happened at all.
//
// It only looks when the offset actually grew across the gap, so an ordinary advance does no extra
// work. The schedule is then re-read in UTC, which has no transitions, to learn the wall clock the
// expression would have picked had the clocks not moved. Interpreting that wall clock back in the
// real zone is what answers the question: Go resolves a local time that does not exist to the
// instant the jump landed on, so a slot inside the lost hour comes back as the transition itself,
// and a slot outside it comes back unchanged and is left alone.
func skippedBySpringForward(spec string, after, next time.Time) (time.Time, bool) {
	loc := next.Location()
	_, afterOffset := after.In(loc).Zone()
	_, nextOffset := next.In(loc).Zone()
	if nextOffset <= afterOffset {
		return time.Time{}, false
	}
	shadow, err := cron.ParseStandard(utcSpec(spec))
	if err != nil {
		return time.Time{}, false
	}
	local := after.In(loc)
	asUTC := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(),
		local.Second(), local.Nanosecond(), time.UTC)
	wall := shadow.Next(asUTC)
	if wall.IsZero() {
		return time.Time{}, false
	}
	// Whether that wall clock exists in the real zone. Reading it back is only the test, never the
	// answer: Go resolves a time the jump erased by applying an offset, and which side it lands on is
	// not something to depend on. Here it lands an hour before the jump, which is earlier than the
	// schedule asked for rather than later.
	probe := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(),
		wall.Second(), wall.Nanosecond(), loc)
	if probe.Hour() == wall.Hour() && probe.Minute() == wall.Minute() {
		return time.Time{}, false
	}
	jump := transitionInstant(after, next, loc, afterOffset)
	if jump.IsZero() || !jump.After(after) || !jump.Before(next) {
		return time.Time{}, false
	}
	return jump, true
}

// transitionInstant returns the first second between after and next whose zone offset is no longer
// the one in force at after, which is the moment the clocks jumped.
//
// A binary search rather than a table lookup, because the standard library exposes no transition
// list. Second granularity is enough: a zone change lands on a whole second, and a cron slot is
// minute-granular.
func transitionInstant(after, next time.Time, loc *time.Location, beforeOffset int) time.Time {
	lo, hi := after.In(loc).Truncate(time.Second), next.In(loc).Truncate(time.Second)
	if _, off := hi.Zone(); off == beforeOffset {
		return time.Time{}
	}
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, off := mid.Zone(); off == beforeOffset {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// utcSpec returns spec with its zone descriptor replaced by UTC, so the same expression can be read
// as wall clocks with no transitions in them.
func utcSpec(spec string) string {
	trimmed := strings.TrimSpace(spec)
	if rest, ok := strings.CutPrefix(trimmed, "CRON_TZ="); ok {
		if _, expr, found := strings.Cut(rest, " "); found {
			return "CRON_TZ=UTC " + strings.TrimSpace(expr)
		}
		return trimmed
	}
	return "CRON_TZ=UTC " + trimmed
}

// sameLocalMinute reports whether two instants fall in the same wall-clock minute of the same zone
// while being different instants, which happens only where a zone rewinds.
func sameLocalMinute(a, b time.Time) bool {
	if a.Equal(b) {
		return false
	}
	loc := a.Location()
	x, y := a.In(loc), b.In(loc)
	return x.Year() == y.Year() && x.Month() == y.Month() && x.Day() == y.Day() &&
		x.Hour() == y.Hour() && x.Minute() == y.Minute()
}
