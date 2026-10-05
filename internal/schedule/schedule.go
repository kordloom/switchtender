// Package schedule holds recurring run schedules and the logic that fires them on a cron cadence or
// an RFC 5545 recurrence. A schedule fires a single run, a split, or a pipeline through the same
// submit paths a client uses, so scheduled work is indistinguishable from manual work once it
// lands.
package schedule

import (
	"errors"
	"fmt"
	"slices"
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
	// ErrBadTimezone is a timezone this system does not know, kept distinct from ErrBadCron so a
	// typo in the zone is never reported as a fault in the expression.
	ErrBadTimezone = errors.New("bad timezone")
	// ErrBadSpringForward is a spring-forward setting other than jump, later, or skip.
	ErrBadSpringForward = errors.New("bad spring-forward setting")
)

// Spring-forward settings say what a schedule does with a time that does not exist because the
// clocks went forward over it, such as 02:30 on the night 02:00 becomes 03:00. A schedule that
// names none takes the default for its kind.
const (
	// SpringForwardJump fires at the instant the clock jumps. It is the default for a cron
	// expression.
	SpringForwardJump = "jump"
	// SpringForwardLater fires the length of the jump later, the time read with the offset in force
	// before it, so a 02:30 fires at 03:30. It is the default for a recurrence rule, and it is how
	// AWX and RFC 5545 read one.
	SpringForwardLater = "later"
	// SpringForwardSkip does not fire that night at all.
	SpringForwardSkip = "skip"
)

// Schedule is a recurring run definition. It fires a pipeline when Steps is set, a split when Shards
// is two or more, and otherwise a single run.
type Schedule struct {
	// ID is the unique schedule identifier.
	ID string `json:"id"`
	// Name identifies the schedule.
	Name string `json:"name"`
	// Cron is the cron expression that sets the cadence. Empty when RRule sets it instead.
	Cron string `json:"cron"`
	// RRule is an RFC 5545 recurrence that sets the cadence instead of Cron: a DTSTART, one or more
	// RRULE lines, and any EXRULE, RDATE, and EXDATE lines. It says what a cron expression cannot,
	// such as the last Friday of each quarter or every other Tuesday, and it is the form AWX stores
	// its schedules in. A schedule carries one of Cron and RRule, never both.
	RRule string `json:"rrule,omitempty"`
	// Timezone is the IANA name, such as America/New_York, the cron expression is read in. A named
	// zone makes "0 9 * * 1" mean nine in that zone through daylight saving changes, not nine
	// wherever the server happens to sit. A recurrence whose DTSTART names a zone is read in that
	// zone, and the two must agree when both are set. A schedule created or imported without one is
	// pinned to the server's zone by name, so every server reads it alike, and a row that still names
	// none is read in UnnamedZone.
	Timezone string `json:"timezone,omitempty"`
	// SpringForward says what happens to a time the clocks skip on the night they go forward:
	// jump fires when the clock jumps, later fires the length of the jump later, and skip does not
	// fire that night. Empty takes the default for the cadence, jump for a cron expression and
	// later for a recurrence rule. A time that happens twice, on the night the clocks go back,
	// fires once at the first of the two whatever this says.
	SpringForward string `json:"spring_forward,omitempty"`
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
	// CreatedBy names the actor who created the schedule, empty for one that predates the field or
	// was created before any authentication existed.
	//
	// It changes nothing about when the schedule fires. A schedule is organization infrastructure
	// rather than one person's property, so deleting an account deliberately does not stop the work
	// that account set up: halting production automation the moment somebody is offboarded is its
	// own outage. What was missing is the record of who set it up, which is what an offboarding
	// review actually needs in order to decide, so it is recorded and left at that.
	CreatedBy string `json:"created_by,omitempty"`
	// LastRunAt is when the schedule last fired.
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// LastRunID is the run created by the most recent fire that created one.
	LastRunID string `json:"last_run_id,omitempty"`
	// LastError is why the most recent fire started no run, and is empty when it started one. A fire
	// that failed used to move LastRunAt and nothing else, so a schedule that had not started a run
	// in weeks looked exactly like one that ran on time.
	LastError string `json:"last_error,omitempty"`
	// LastSkip is why the most recent fire was skipped, such as no hosts matched, and is empty when
	// it was not. A skip is not a failure: the schedule fired on time and found nothing to reach.
	LastSkip string `json:"last_skip,omitempty"`
	// SkippedFires counts the fires in a row, ending with the most recent, that were skipped. A fire
	// that starts a run or fails resets it.
	SkippedFires int `json:"skipped_fires,omitempty"`
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
		// A step carries its own dependency slice, and copying the step copies the slice header
		// rather than what it points at. Left shared, a handler that edits a dependency on what the
		// store handed it rewrites the stored pipeline's graph, so the step order of a scheduled
		// pipeline changes with nothing saved and nothing recorded.
		for i := range out.Steps {
			if len(out.Steps[i].DependsOn) > 0 {
				out.Steps[i].DependsOn = slices.Clone(out.Steps[i].DependsOn)
			}
		}
	}
	return &out
}

// Validate reports whether the schedule has a parseable cron or recurrence, a valid timezone, and a
// target to run. A recurrence that has already fired its last time does not validate, since storing
// it would create a schedule that never fires.
func (s *Schedule) Validate() error {
	return s.ValidateAt(time.Now())
}

// ValidateAt is Validate with now standing in for the current time, which decides whether a
// recurrence has already fired its last time. An import passes the time it is converting at, so the
// answer depends on the export and that time, never on when the conversion happens to run.
func (s *Schedule) ValidateAt(now time.Time) error {
	if err := s.validTimezone(); err != nil {
		return err
	}
	if err := s.validSpringForward(); err != nil {
		return err
	}
	if s.Cron != "" && s.RRule != "" {
		return fmt.Errorf("%w: a schedule takes a cron expression or a recurrence rule, not both",
			ErrBadRecurrence)
	}
	if _, err := s.NextFire(now); err != nil {
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
			ErrBadTimezone, s.Timezone)
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("%w: unknown timezone %q, use an IANA name such as America/New_York",
			ErrBadTimezone, s.Timezone)
	}
	return nil
}

// validSpringForward reports whether the spring-forward setting is empty or one of the three
// settings, so an unknown one is refused where it is written rather than read as a default.
func (s *Schedule) validSpringForward() error {
	switch s.SpringForward {
	case "", SpringForwardJump, SpringForwardLater, SpringForwardSkip:
		return nil
	}
	return fmt.Errorf("%w: %q is not one of %s, %s, or %s", ErrBadSpringForward, s.SpringForward,
		SpringForwardJump, SpringForwardLater, SpringForwardSkip)
}

// SpringForwardSetting returns what the schedule does with a time the clocks skip: its own setting,
// or when it names none, the default for its cadence, jump for a cron expression and later for a
// recurrence rule.
func (s *Schedule) SpringForwardSetting() string {
	switch {
	case s.SpringForward != "":
		return s.SpringForward
	case s.RRule != "":
		return SpringForwardLater
	}
	return SpringForwardJump
}

// effectiveCron returns the cron expression with the schedule's timezone applied, so the same
// expression fires in the schedule's zone rather than the server's. An expression that carries its
// own zone descriptor is left as written, and one that names no zone anywhere is read in
// UnnamedZone.
//
// The expression with no zone used to be handed to the cron library as it was, and the library
// reads such an expression in the zone of the time it is asked about. The scheduler asks with its
// own clock's time, so each server read the schedule in its own zone, and a highly available pair
// whose servers disagreed fired a daily schedule twice in one day.
func (s *Schedule) effectiveCron() string {
	switch {
	case s.Timezone != "":
		return "CRON_TZ=" + s.Timezone + " " + s.Cron
	case hasZoneDescriptor(s.Cron):
		return s.Cron
	}
	return "CRON_TZ=" + UnnamedZone + " " + s.Cron
}

// hasZoneDescriptor reports whether a cron expression begins with the zone descriptor the cron
// library reads, CRON_TZ= or TZ=.
func hasZoneDescriptor(spec string) bool {
	return strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "TZ=")
}

// NamesZone reports whether the schedule says which zone it is read in: through its timezone, a
// zone descriptor on its cron expression, or a zone on its recurrence's DTSTART.
func (s *Schedule) NamesZone() bool {
	return s.Timezone != "" || hasZoneDescriptor(s.Cron) || RecurrenceZone(s.RRule) != ""
}

// PinZone writes zone onto a schedule that names none, and leaves one that does as it is.
//
// A schedule created or imported without a zone is read in the server's zone, and pinning records
// that zone by name at the moment it is written. Left unnamed, the zone was whichever one the
// server evaluating the schedule sat in, so two servers sharing one database could read the same
// schedule hours apart.
func (s *Schedule) PinZone(zone string) {
	if !s.NamesZone() {
		s.Timezone = zone
	}
}

// NextFire returns the next time this schedule fires after the given time, in its own timezone.
//
// The zone is checked before the expression, so a typo in one is not reported as a fault in the
// other. The cron parser rejects an unknown zone with its own generic message, which every caller
// then rendered as "invalid cron expression": an operator who typed America/New_york was told their
// perfectly good "0 2 * * *" was wrong, and had no reason to look at the field that actually was.
//
// A schedule driven by a recurrence returns ErrExhausted once its COUNT or UNTIL has run out.
func (s *Schedule) NextFire(after time.Time) (time.Time, error) {
	// The preview endpoint builds a Schedule and calls this directly without Validate, so the zone
	// is checked here too rather than only on the save path. validTimezone is the one definition.
	if err := s.validTimezone(); err != nil {
		return time.Time{}, err
	}
	// The same holds for the spring-forward setting: an unknown one is refused, never read as
	// whichever default the cadence would have.
	if err := s.validSpringForward(); err != nil {
		return time.Time{}, err
	}
	if s.RRule != "" {
		return s.nextRecurrence(after)
	}
	return nextCronFire(s.effectiveCron(), after, s.SpringForwardSetting())
}

// nextRecurrence returns the recurrence's next fire after the given time, read in the zone its
// DTSTART names or, for a floating DTSTART, in the schedule's own zone.
//
// A DTSTART that names one zone on a schedule that names another is refused rather than resolved
// in favor of either. Picking one silently is how an imported 02:00 window fires at 02:00 somewhere
// nobody meant, and the two fields disagreeing is a mistake whoever wrote them should see.
func (s *Schedule) nextRecurrence(after time.Time) (time.Time, error) {
	loc, err := s.recurrenceFallback()
	if err != nil {
		return time.Time{}, err
	}
	rc, err := parseRecurrence(s.RRule, loc, s.SpringForwardSetting())
	if err != nil {
		return time.Time{}, err
	}
	if zone := rc.Zone(); zone != "" && s.Timezone != "" && zone != s.Timezone {
		return time.Time{}, fmt.Errorf("%w: the rule's DTSTART is in %s and the schedule's "+
			"timezone is %s; make them agree or leave the timezone empty", ErrBadRecurrence, zone,
			s.Timezone)
	}
	return rc.Next(after)
}

// recurrenceFallback returns the zone a floating DTSTART is read in: the schedule's timezone, or
// UnnamedZone when it names none. The server's own zone stood here, which made the same stored rule
// fire at different instants on servers in different zones.
func (s *Schedule) recurrenceFallback() (*time.Location, error) {
	zone := s.Timezone
	if zone == "" {
		zone = UnnamedZone
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown timezone %q", ErrBadTimezone, zone)
	}
	return loc, nil
}

// NextFireAfter returns the time the schedule fires next once a tick at now has fired its
// occurrence due: the first fire after now that is not the second reading of a wall clock time this
// fire already covered.
//
// On the night the clocks go back a wall clock time happens twice, and a schedule documents that
// such a time fires once, at the first of the two. NextFire guards that by skipping a fire that
// shares a wall clock minute with the time it is asked about, which holds only while the tick lands
// inside the minute it fired. A tick a minute or more late, from a restart, a database failover, a
// suspended host, or a tick interval longer than a minute, asked from a later minute, so the guard
// let the second 01:30 through and a nightly job ran twice on one night. The fire covered every
// reading from due to now, so a next fire whose wall clock reading already happened in that
// stretch is the repeat and is skipped.
func (s *Schedule) NextFireAfter(due, now time.Time) (time.Time, error) {
	next, err := s.NextFire(now)
	if err != nil || s.RRule != "" || due.After(now) {
		return next, err
	}
	loc, interval, lerr := s.cronClock()
	if lerr != nil || interval {
		return next, nil
	}
	for range maxRepeatSkips {
		twin, ok := earlierReading(next, loc)
		if !ok || twin.Before(due) || twin.After(now) {
			return next, nil
		}
		if next, err = s.NextFire(next); err != nil {
			return next, err
		}
	}
	return next, nil
}

// maxRepeatSkips bounds how many repeated readings NextFireAfter steps over. One night repeats an
// hour at most a couple of times over in any zone, so a schedule that keeps landing on repeats past
// this is not one the loop should spin on.
const maxRepeatSkips = 128

// cronClock returns the zone a cron schedule's wall clock is read in, and whether the expression is
// an interval, which counts elapsed time and has no wall clock to repeat.
func (s *Schedule) cronClock() (*time.Location, bool, error) {
	parsed, err := cron.ParseStandard(s.effectiveCron())
	if err != nil {
		return nil, false, err
	}
	if _, interval := parsed.(cron.ConstantDelaySchedule); interval {
		return nil, true, nil
	}
	spec, ok := parsed.(*cron.SpecSchedule)
	if !ok || spec.Location == nil {
		return nil, false, fmt.Errorf("%w: %q has no zone to read it in", ErrBadCron, s.Cron)
	}
	return spec.Location, false, nil
}

// earlierReading returns the earlier instant whose wall clock reading in loc is the same as t's,
// and whether there is one, which happens only inside the stretch of a night the clocks went back.
func earlierReading(t time.Time, loc *time.Location) (time.Time, bool) {
	local := t.In(loc)
	_, offset := local.Zone()
	start, _ := local.ZoneBounds()
	if start.IsZero() {
		return time.Time{}, false
	}
	_, before := start.Add(-time.Second).In(loc).Zone()
	if before <= offset {
		return time.Time{}, false
	}
	twin := t.Add(time.Duration(offset-before) * time.Second)
	if !twin.Before(start) {
		return time.Time{}, false
	}
	return twin, true
}

// NextFire returns the next time the cron expression fires after the given time, firing a time the
// clocks skip at the instant they jump, which is a cron schedule's default.
//
// A parseable expression that can never come due is refused here rather than passed on. The cron
// library gives up after scanning five years and returns the zero time with no error, and the
// scheduler reads any next-run time that is not after now as due. So "0 0 30 2 *", a February the
// thirtieth that will never exist, was stored, read as due on every tick, claimed, fired, and
// rewritten to zero again: one authenticated call produced a run every fifteen seconds forever,
// with nothing logged and no rate limit in front of it.
func NextFire(spec string, after time.Time) (time.Time, error) {
	return nextCronFire(spec, after, SpringForwardJump)
}

// nextCronFire returns the next time the cron expression fires after the given time, placing a
// time the clocks skip by the spring-forward setting.
//
// Both daylight-saving corrections are worked out in the zone the expression is read in. They used
// to read the zone off the time they were handed, which is the caller's and not the schedule's: a
// server running in UTC, which is how a container runs, asks with UTC times, UTC never moves, and
// so a schedule pinned to America/Chicago fired twice on the night the clocks went back and not at
// all on the night they went forward, while every test, written in the schedule's own zone, passed.
// The answer is handed back in the caller's zone, which is what the cron library does.
func nextCronFire(spec string, after time.Time, setting string) (time.Time, error) {
	if err := checkSpec(spec); err != nil {
		return time.Time{}, err
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrBadCron, err)
	}
	next := sched.Next(after)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: %q parses but never comes due, so it would be read as "+
			"due on every tick", ErrBadCron, spec)
	}
	// An interval schedule counts elapsed real time from the last fire, so it has no wall clock to
	// repeat or to lose and neither daylight-saving correction below applies to it. Both are written
	// for minute-granular cron slots, and an interval is not minute-granular: this parser accepts
	// intervals well under a minute, so the repeat guard read every second fire of "@every 30s" as a
	// zone rewind and dropped it, every day of the year rather than the one it was written for.
	if _, interval := sched.(cron.ConstantDelaySchedule); interval {
		return next, nil
	}
	zone := cronZone(sched, after)
	// On the day a zone falls back, the same local minute comes round twice, an hour apart, and the
	// cron library returns both. The scheduler advances from the moment it fired, so a nightly job
	// inside the repeated hour fired, advanced to the same wall-clock minute in the new offset, and
	// fired again: two full executions of the same non-idempotent playbook on one nominal day. A
	// cron slot is minute-granular, so two distinct instants sharing a local minute can only be that
	// repeat, and the second one is skipped. Every spring-forward setting keeps this.
	if sameLocalMinute(next.In(zone), after.In(zone)) {
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
	// 02:00 is not a time. That is the default; the schedule's setting can choose otherwise.
	return cronSpringForward(spec, zone, after, next, setting).In(after.Location()), nil
}

// checkSpec refuses the two expressions the cron parser does not turn away on its own, before it is
// handed anything. Both are caller-supplied text that reaches here from the schedule preview and
// from schedule creation, and a stored row carrying either is read again on every tick.
func checkSpec(spec string) error {
	expr, err := withoutZoneDescriptor(spec)
	if err != nil {
		return err
	}
	return checkInterval(expr)
}

// withoutZoneDescriptor returns the expression with a leading zone descriptor removed, and refuses
// a descriptor that names no expression at all.
//
// The cron parser finds the end of the descriptor by looking for the first space, and with no space
// anywhere it slices to a negative index and panics rather than returning an error. So "TZ=UTC", a
// string a person could easily paste into the preview, crashed the process, and a row holding one
// took the scheduler goroutine down on every tick after every restart. A zone on its own is not a
// cadence in any case, so it is refused here and never reaches the parser.
func withoutZoneDescriptor(spec string) (string, error) {
	for _, prefix := range []string{"CRON_TZ=", "TZ="} {
		body, ok := strings.CutPrefix(spec, prefix)
		if !ok {
			continue
		}
		_, expr, found := strings.Cut(body, " ")
		if !found {
			return "", fmt.Errorf("%w: %q names a timezone and no cadence, so there is nothing "+
				"to fire", ErrBadCron, spec)
		}
		return strings.TrimSpace(expr), nil
	}
	return spec, nil
}

// checkInterval refuses an interval expression that names a duration of zero or less.
//
// The cron library clamps any duration under a second up to one second rather than complaining, so
// "@every 0s" and "@every -1h" parsed, were stored, and then came due on every tick for as long as
// the schedule existed: one authenticated call producing a run every fifteen seconds forever. That
// is the same harm as an expression which never comes due, arriving from the other direction, and
// neither value can be what anybody meant. A duration that does not parse at all is left to the
// library, which names what is wrong with it.
func checkInterval(expr string) error {
	raw, ok := strings.CutPrefix(expr, "@every ")
	if !ok {
		return nil
	}
	every, err := time.ParseDuration(raw)
	if err != nil {
		return nil
	}
	if every <= 0 {
		return fmt.Errorf("%w: an interval must be positive, and %q would be read as due on every "+
			"tick", ErrBadCron, expr)
	}
	return nil
}

// skippedBySpringForward reports the instant to fire at when loc, the zone the expression is read
// in, jumped over the scheduled wall clock between after and next, and whether that happened at
// all.
//
// It only looks when the offset actually grew across the gap, so an ordinary advance does no extra
// work. The schedule is then re-read in UTC, which has no transitions, to learn the wall clock the
// expression would have picked had the clocks not moved. Interpreting that wall clock back in the
// real zone is what answers the question: Go resolves a local time that does not exist to the
// instant the jump landed on, so a slot inside the lost hour comes back as the transition itself,
// and a slot outside it comes back unchanged and is left alone.
func skippedBySpringForward(spec string, loc *time.Location, after, next time.Time) (time.Time, bool) {
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
