package schedule

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/robfig/cron/v3"
)

// jumpLookback is how far before the asked-about time the search for a clock jump starts. A time
// the clocks skipped is fired at most the length of the jump after the jump itself, and no zone
// jumps by more than a few hours, so a jump further back than this cannot place a fire after that
// time.
const jumpLookback = 36 * time.Hour

// cronZone returns the zone a parsed cron expression is read in: the zone its descriptor names, or
// for an expression with none, the zone of the time it is asked about, which is how the cron
// library reads one.
func cronZone(sched cron.Schedule, after time.Time) *time.Location {
	if spec, ok := sched.(*cron.SpecSchedule); ok && spec.Location != nil &&
		spec.Location != time.Local {
		return spec.Location
	}
	return after.Location()
}

// cronSpringForward returns the instant a cron expression read in loc fires at after the given
// time, given next, the answer the cron library gave. The library steps over a wall time the clocks
// skipped, which is the skip setting as it stands. The jump setting fires at the instant the clock
// jumped instead, and the later setting reads the skipped time with the offset in force before the
// jump, as AWX reads a recurrence rule.
func cronSpringForward(spec string, loc *time.Location, after, next time.Time, setting string) time.Time {
	switch setting {
	case SpringForwardSkip:
	case SpringForwardLater:
		if shifted, ok := shiftedBySpringForward(spec, loc, after, next); ok {
			return shifted
		}
	default:
		if jumped, ok := skippedBySpringForward(spec, loc, after, next); ok {
			return jumped
		}
	}
	return next
}

// shiftedBySpringForward reports the instant to fire at under the later setting: the earliest wall
// clock the expression names that loc skipped, read with the offset in force before the jump, that
// lands after after and no later than next, and whether there is one.
//
// A time read that way lands inside the stretch just after the jump, so a skipped slot can land
// after real slots that come once the clocks have moved, and a skipped slot can land on the same
// instant as a real one, which then fires once. Every jump near the answer is asked on its own, for
// the first skipped slot landing after the asked-about time, rather than only a jump lying between
// that time and next: the second of two skipped slots lands after the first has fired, and by then
// the time the scheduler asks about is already past the jump.
func shiftedBySpringForward(spec string, loc *time.Location, after, next time.Time) (time.Time, bool) {
	shadow, err := cron.ParseStandard(utcSpec(spec))
	if err != nil {
		return time.Time{}, false
	}
	var best time.Time
	periods := zonePeriods(loc, after.Add(-jumpLookback).Unix(), next.Unix())
	for i := 0; i+1 < len(periods); i++ {
		before, jumped := periods[i], periods[i+1]
		if jumped.offset <= before.offset {
			continue
		}
		// The wall clock readings the jump erased run from first up to last, held as UTC readings.
		first := jumped.start + int64(before.offset)
		last := jumped.start + int64(jumped.offset)
		// A skipped reading lands after the asked-about time when it is past that time's own
		// reading in the offset before the jump. The shadow schedule answers strictly after what it
		// is given.
		from := max(first-1, after.Unix()+int64(before.offset))
		wall := shadow.Next(time.Unix(from, 0).UTC())
		if wall.IsZero() || wall.Unix() >= last {
			continue
		}
		at := time.Unix(wall.Unix()-int64(before.offset), 0).In(loc)
		if !at.After(after) || at.After(next) {
			continue
		}
		if best.IsZero() || at.Before(best) {
			best = at
		}
	}
	return best, !best.IsZero()
}

// placeWall places a zone-free wall clock in loc by the rules RFC 5545 gives for the two
// daylight-saving edges, except where setting says otherwise. A wall time that occurs twice is the
// first of the two under every setting. One that does not occur is read with the offset in force
// before the jump under later, which lands it the length of the jump later; it is placed at the
// instant the clock jumped under jump; and under skip it is not placed at all, which the second
// result reports.
//
// The standard library is not asked to decide either case. time.Date documents that it picks one of
// the two possible instants without saying which, and for a wall time inside the jump it lands an
// hour early in practice, which would fire a 02:30 rule at 01:30 on the night clocks go forward.
func placeWall(wall time.Time, loc *time.Location, setting string) (time.Time, bool) {
	sec := wall.Unix()
	periods := zonePeriods(loc, sec-36*3600, sec+36*3600)
	found := int64(math.MaxInt64)
	for _, p := range periods {
		at := sec - int64(p.offset)
		if at >= p.start && at < p.end && at < found {
			found = at
		}
	}
	if found != math.MaxInt64 {
		return time.Unix(found, 0).In(loc), true
	}
	for i := 0; i+1 < len(periods); i++ {
		before, jump := periods[i], periods[i+1]
		if sec >= jump.start+int64(before.offset) && sec < jump.start+int64(jump.offset) {
			switch setting {
			case SpringForwardSkip:
				return time.Time{}, false
			case SpringForwardJump:
				return time.Unix(jump.start, 0).In(loc), true
			}
			return time.Unix(sec-int64(before.offset), 0).In(loc), true
		}
	}
	y, mo, d := wall.Date()
	h, mi, s := wall.Clock()
	return time.Date(y, mo, d, h, mi, s, 0, loc), true
}

const (
	// gapHorizon is how far ahead NextSpringGap looks for a night the clocks go forward.
	gapHorizon = 400 * 24 * time.Hour
	// gapWalkLimit bounds how many fires one setting is walked through in the stretch after a jump,
	// which a schedule firing every minute fills with a hundred and twenty at most.
	gapWalkLimit = 200
	// gapFiresShown bounds how many of those fires a SpringGap lists.
	gapFiresShown = 12
)

// SpringGap is what a schedule does on the next night the clocks go forward over a time it names:
// when the clocks jump, the wall clock readings they jump between, the setting in force, and the
// real instants the schedule fires in the stretch the jump moves a skipped time into.
type SpringGap struct {
	// Transition is the instant the clocks jump forward.
	Transition time.Time `json:"transition"`
	// Zone names the zone that jumps.
	Zone string `json:"zone"`
	// From is the wall clock reading the jump leaves, such as 02:00. It and every reading up to To
	// do not exist that night.
	From string `json:"from"`
	// To is the wall clock reading the jump lands on, such as 03:00.
	To string `json:"to"`
	// Setting is the spring-forward setting in force: jump, later, or skip.
	Setting string `json:"setting"`
	// Fires are the real instants the schedule fires under the setting from the jump until as long
	// after it as the jump is wide, at most a dozen. Empty when nothing fires in that stretch.
	Fires []time.Time `json:"fires,omitempty"`
	// MoreFires counts the fires in that stretch past the ones listed.
	MoreFires int `json:"more_fires,omitempty"`
}

// NextSpringGap returns what the schedule does on the first night within about thirteen months of
// the given time that the clocks go forward over a time it names, or nil when no such night comes.
// A night is reported only when the three spring-forward settings would fire the schedule at
// different instants that night, so a schedule the setting changes nothing for, such as one firing
// every hour, is not reported at all.
func (s *Schedule) NextSpringGap(after time.Time) (*SpringGap, error) {
	if err := s.validTimezone(); err != nil {
		return nil, err
	}
	if err := s.validSpringForward(); err != nil {
		return nil, err
	}
	loc, err := s.wallZone()
	if err != nil {
		return nil, err
	}
	periods := zonePeriods(loc, after.Unix(), after.Add(gapHorizon).Unix())
	for i := 0; i+1 < len(periods); i++ {
		before, jumped := periods[i], periods[i+1]
		if jumped.offset <= before.offset || jumped.start <= after.Unix() ||
			jumped.start > after.Add(gapHorizon).Unix() {
			continue
		}
		start := time.Unix(jumped.start, 0).In(loc)
		end := start.Add(time.Duration(jumped.offset-before.offset) * time.Second)
		bySetting := map[string][]time.Time{}
		for _, setting := range []string{SpringForwardJump, SpringForwardLater, SpringForwardSkip} {
			fires, werr := s.firesBetween(setting, start, end)
			if werr != nil {
				return nil, werr
			}
			bySetting[setting] = fires
		}
		if sameInstants(bySetting[SpringForwardJump], bySetting[SpringForwardSkip]) &&
			sameInstants(bySetting[SpringForwardLater], bySetting[SpringForwardSkip]) {
			continue
		}
		setting := s.SpringForwardSetting()
		fires := bySetting[setting]
		gap := &SpringGap{
			Transition: start, Zone: loc.String(),
			From:    wallReading(jumped.start + int64(before.offset)),
			To:      wallReading(jumped.start + int64(jumped.offset)),
			Setting: setting,
		}
		if len(fires) > gapFiresShown {
			gap.MoreFires = len(fires) - gapFiresShown
			fires = fires[:gapFiresShown]
		}
		gap.Fires = fires
		return gap, nil
	}
	return nil, nil
}

// wallZone returns the zone the schedule's wall clock times are read in, the same one NextFire
// reads them in: its timezone, the zone a recurrence rule's DTSTART or a cron expression's
// descriptor names, or UnnamedZone for a schedule naming none. An interval has no wall clock and
// answers its timezone or UnnamedZone.
func (s *Schedule) wallZone() (*time.Location, error) {
	if s.RRule != "" {
		loc, err := s.recurrenceFallback()
		if err != nil {
			return nil, err
		}
		rc, err := ParseRecurrence(s.RRule, loc)
		if err != nil {
			return nil, err
		}
		return rc.loc, nil
	}
	loc, interval, err := s.cronClock()
	if err != nil {
		return nil, err
	}
	if interval {
		return s.recurrenceFallback()
	}
	return loc, nil
}

// firesBetween returns the instants the schedule fires under setting from start, inclusive, up to
// end, exclusive, at most gapWalkLimit of them. A recurrence that runs out on the way simply stops.
func (s *Schedule) firesBetween(setting string, start, end time.Time) ([]time.Time, error) {
	probe := s.Clone()
	probe.SpringForward = setting
	var out []time.Time
	at := start.Add(-time.Second)
	for len(out) < gapWalkLimit {
		next, err := probe.NextFire(at)
		if errors.Is(err, ErrExhausted) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !next.Before(end) {
			break
		}
		out = append(out, next)
		at = next
	}
	return out, nil
}

// sameInstants reports whether two lists hold the same instants in the same order.
func sameInstants(a, b []time.Time) bool {
	return slices.EqualFunc(a, b, func(x, y time.Time) bool { return x.Equal(y) })
}

// wallReading renders a UTC reading, a wall clock held as Unix seconds, as the hour and minute it
// names, with the seconds only when there are any.
func wallReading(sec int64) string {
	t := time.Unix(sec, 0).UTC()
	if t.Second() != 0 {
		return t.Format("15:04:05")
	}
	return t.Format("15:04")
}
