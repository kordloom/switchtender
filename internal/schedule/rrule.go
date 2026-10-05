package schedule

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

var (
	// ErrBadRecurrence is returned when an RFC 5545 recurrence cannot be parsed or names something
	// this scheduler does not evaluate.
	ErrBadRecurrence = errors.New("bad recurrence")
	// ErrExhausted is returned when a recurrence bounded by COUNT or UNTIL has no fire left after
	// the time asked about. It is distinct from ErrBadRecurrence because a finished rule is not a
	// broken one: the scheduler fires its last occurrence and then stops, where a broken rule is
	// never stored.
	ErrExhausted = errors.New("recurrence has no fire left")
)

// Limits on what one recurrence may hold. A stored rule is evaluated on every scheduler tick, so
// its size and the work one evaluation may do are bounded where it is written rather than trusted.
const (
	// maxRecurrenceBytes bounds the stored text.
	maxRecurrenceBytes = 8 << 10
	// maxRules bounds the RRULE and EXRULE lines together.
	maxRules = 16
	// maxDates bounds the RDATE and EXDATE values together.
	maxDates = 1000
	// maxCount bounds COUNT. AWX accepts at most 999, so every rule it writes fits.
	maxCount = 10000
	// maxInterval bounds INTERVAL, which keeps the fast-forward arithmetic far from overflow.
	maxInterval = 100000
	// maxExamined bounds how many occurrences one evaluation looks at, including the ones at or
	// before the asked-about time, before it gives up. Only a rule whose BY parts expand it enormously,
	// or whose exclusions remove nearly everything it generates, gets near it, and such a rule is
	// refused rather than allowed to spin on every tick.
	maxExamined = 200000
	// maxByProduct bounds how many occurrences a rule's BY parts place in one period. A rule that
	// names every second, minute, hour, day, and month places tens of millions per period, which the
	// library buffers before it yields one, so a document a few hundred bytes long can exhaust the
	// process. A real schedule's BY parts are short, so this sits far above any of them.
	maxByProduct = 100000
	// fastForwardMargin is how far before the asked-about time a fast-forwarded start is placed, so
	// the shifted rule still generates every occurrence near that time whatever the zone's offset.
	fastForwardMargin = 3 * 24 * time.Hour
)

// ruleKeys are the RRULE parts this evaluator reads. Everything RFC 5545 defines is here except
// FREQ=SECONDLY, which is refused separately, and nothing outside RFC 5545 is.
var ruleKeys = []string{
	"FREQ", "INTERVAL", "COUNT", "UNTIL", "WKST", "BYSETPOS", "BYMONTH", "BYMONTHDAY",
	"BYYEARDAY", "BYWEEKNO", "BYDAY", "BYHOUR", "BYMINUTE", "BYSECOND",
}

// Recurrence is a parsed RFC 5545 recurrence set: a DTSTART, one or more RRULE lines, and any
// EXRULE, RDATE, and EXDATE lines, read in one timezone.
//
// Rules are expanded on the wall clock, the way RFC 5545 and the dateutil library AWX evaluates its
// schedules with both do, and every generated wall time is then placed in the zone by the rules RFC
// 5545 gives for the two daylight-saving edges. A wall time that occurs twice, on the night clocks
// go back, is the first of the two, so a nightly rule inside the repeated hour fires once. A wall
// time that does not exist, on the night clocks go forward, is read with the offset in force before
// the gap, so a 02:30 rule fires at 03:30 that night rather than being dropped. Expanding on the
// wall clock is what keeps a 09:00 rule at 09:00 local through every transition.
type Recurrence struct {
	// loc is the zone the wall clock is read in.
	loc *time.Location
	// zone names the zone the DTSTART declared: its TZID, UTC for the Z form, or empty for a floating
	// time that takes the schedule's own zone.
	zone string
	// start is the DTSTART wall clock, held in UTC as a zone-free reading.
	start time.Time
	// include are the RRULE lines.
	include []bound
	// exclude are the EXRULE lines.
	exclude []bound
	// rdates are the RDATE instants.
	rdates []time.Time
	// exdates are the EXDATE instants.
	exdates []time.Time
	// gap is the spring-forward setting a wall time the clocks skip is placed by: jump, later, or
	// skip. Later is RFC 5545's reading and the default.
	gap string
}

// bound is one RRULE or EXRULE with its UNTIL held as a real instant, since the expansion itself
// runs on a zone-free wall clock and cannot compare against one.
type bound struct {
	// opt is the parsed rule, its DTSTART set to the recurrence's wall clock and its UNTIL cleared.
	opt rrule.ROption
	// until is the instant past which the rule generates nothing, zero when it is unbounded.
	until time.Time
}

// ParseRecurrence parses an RFC 5545 recurrence set. Properties are separated by line breaks or
// spaces, the two layouts AWX writes. A bare FREQ=... line is read as an RRULE. A DTSTART that
// names no zone is read in fallback, which is the schedule's own timezone.
func ParseRecurrence(text string, fallback *time.Location) (*Recurrence, error) {
	return parseRecurrence(text, fallback, SpringForwardLater)
}

// parseRecurrence is ParseRecurrence with the spring-forward setting a wall time the clocks skip is
// placed by, for the RRULE, EXRULE, RDATE, and EXDATE lines alike, so an exclusion still matches
// the occurrence it names whichever setting the schedule carries.
func parseRecurrence(text string, fallback *time.Location, setting string) (*Recurrence, error) {
	if fallback == nil {
		fallback = time.Local
	}
	if len(text) > maxRecurrenceBytes {
		return nil, fmt.Errorf("%w: a recurrence may be at most %d bytes", ErrBadRecurrence,
			maxRecurrenceBytes)
	}
	rc := &Recurrence{loc: fallback, gap: setting}
	var dtstart string
	var rules, exrules []string
	var rdates, exdates []string
	for field := range strings.FieldsSeq(text) {
		if strings.HasPrefix(strings.ToUpper(field), "FREQ=") {
			rules = append(rules, field)
			continue
		}
		cut := strings.IndexAny(field, ";:")
		if cut <= 0 {
			return nil, fmt.Errorf("%w: %q is not a recurrence property", ErrBadRecurrence, clip(field))
		}
		name := strings.ToUpper(field[:cut])
		switch name {
		case "DTSTART":
			if dtstart != "" {
				return nil, fmt.Errorf("%w: a recurrence has one DTSTART", ErrBadRecurrence)
			}
			dtstart = field
		case "RRULE", "EXRULE":
			_, value, ok := strings.Cut(field, ":")
			if !ok || value == "" {
				return nil, fmt.Errorf("%w: %s names no rule", ErrBadRecurrence, name)
			}
			if name == "RRULE" {
				rules = append(rules, value)
			} else {
				exrules = append(exrules, value)
			}
		case "RDATE":
			rdates = append(rdates, field)
		case "EXDATE":
			exdates = append(exdates, field)
		default:
			return nil, fmt.Errorf("%w: %s is not a property this scheduler reads; use DTSTART, "+
				"RRULE, EXRULE, RDATE, or EXDATE", ErrBadRecurrence, clip(name))
		}
	}
	if dtstart == "" {
		return nil, fmt.Errorf("%w: a recurrence needs a DTSTART, such as "+
			"DTSTART;TZID=America/New_York:20260105T090000, to say when it starts and at what time "+
			"of day", ErrBadRecurrence)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%w: a recurrence needs at least one RRULE", ErrBadRecurrence)
	}
	if len(rules)+len(exrules) > maxRules {
		return nil, fmt.Errorf("%w: a recurrence may hold at most %d rules", ErrBadRecurrence,
			maxRules)
	}
	if err := rc.parseStart(dtstart); err != nil {
		return nil, err
	}
	for _, text := range rules {
		b, err := rc.parseRule(text)
		if err != nil {
			return nil, err
		}
		rc.include = append(rc.include, b)
	}
	for _, text := range exrules {
		b, err := rc.parseRule(text)
		if err != nil {
			return nil, err
		}
		rc.exclude = append(rc.exclude, b)
	}
	for _, field := range rdates {
		dates, err := rc.parseDates(field)
		if err != nil {
			return nil, err
		}
		rc.rdates = append(rc.rdates, dates...)
	}
	for _, field := range exdates {
		dates, err := rc.parseDates(field)
		if err != nil {
			return nil, err
		}
		rc.exdates = append(rc.exdates, dates...)
	}
	if len(rc.rdates)+len(rc.exdates) > maxDates {
		return nil, fmt.Errorf("%w: a recurrence may list at most %d dates", ErrBadRecurrence,
			maxDates)
	}
	return rc, nil
}

// Zone names the zone the DTSTART declared: its TZID, UTC for the Z form, or empty for a floating
// time read in the schedule's own zone.
func (rc *Recurrence) Zone() string { return rc.zone }

// RecurrenceZone returns the zone a recurrence's DTSTART declares, or empty when it declares none
// or does not parse. It lets a caller fill a schedule's timezone from the rule it was given.
func RecurrenceZone(text string) string {
	rc, err := ParseRecurrence(text, time.UTC)
	if err != nil {
		return ""
	}
	return rc.zone
}

// parseStart reads the DTSTART property: its zone and its wall clock.
func (rc *Recurrence) parseStart(field string) error {
	params, value, ok := strings.Cut(field, ":")
	if !ok || value == "" {
		return fmt.Errorf("%w: DTSTART names no time", ErrBadRecurrence)
	}
	loc, named, err := rc.paramZone(params)
	if err != nil {
		return err
	}
	wall, utc, err := parseWall(value)
	if err != nil {
		return err
	}
	switch {
	case utc && named != "":
		return fmt.Errorf("%w: DTSTART names the zone %s and also ends in Z, which is UTC; use "+
			"one or the other", ErrBadRecurrence, named)
	case utc:
		rc.loc, rc.zone = time.UTC, "UTC"
	case named != "":
		rc.loc, rc.zone = loc, named
	}
	rc.start = wall
	return nil
}

// paramZone reads a TZID parameter from a property's parameters and loads it. It returns an empty
// name when there is none, and refuses any parameter other than TZID and a VALUE of DATE-TIME or
// DATE.
func (rc *Recurrence) paramZone(params string) (*time.Location, string, error) {
	parts := strings.Split(params, ";")
	var loc *time.Location
	name := ""
	for _, param := range parts[1:] {
		key, value, ok := strings.Cut(param, "=")
		if !ok {
			return nil, "", fmt.Errorf("%w: %q is not a parameter", ErrBadRecurrence, clip(param))
		}
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "TZID":
			value = strings.TrimSpace(value)
			l, err := time.LoadLocation(value)
			if err != nil || value == "" || strings.EqualFold(value, "local") {
				return nil, "", fmt.Errorf("%w: unknown timezone %q in TZID, use an IANA name such "+
					"as America/New_York", ErrBadTimezone, clip(value))
			}
			loc, name = l, value
		case "VALUE":
			switch strings.ToUpper(strings.TrimSpace(value)) {
			case "DATE-TIME", "DATE":
			default:
				return nil, "", fmt.Errorf("%w: VALUE=%s is not supported, only DATE-TIME and DATE",
					ErrBadRecurrence, clip(value))
			}
		default:
			return nil, "", fmt.Errorf("%w: the parameter %s is not supported", ErrBadRecurrence,
				clip(key))
		}
	}
	return loc, name, nil
}

// parseRule reads one RRULE or EXRULE value into a bound rule.
func (rc *Recurrence) parseRule(text string) (bound, error) {
	seen := map[string]bool{}
	untilRaw := ""
	for part := range strings.SplitSeq(text, ";") {
		key, value, ok := strings.Cut(part, "=")
		key = strings.ToUpper(strings.TrimSpace(key))
		if !ok || value == "" {
			return bound{}, fmt.Errorf("%w: %q in %q is not a KEY=VALUE pair", ErrBadRecurrence,
				clip(part), clip(text))
		}
		if !slices.Contains(ruleKeys, key) {
			return bound{}, fmt.Errorf("%w: %s is not a rule part this scheduler reads",
				ErrBadRecurrence, clip(key))
		}
		if seen[key] {
			return bound{}, fmt.Errorf("%w: %s appears twice in one rule", ErrBadRecurrence, key)
		}
		seen[key] = true
		if key == "UNTIL" {
			untilRaw = value
		}
	}
	if seen["COUNT"] && seen["UNTIL"] {
		return bound{}, fmt.Errorf("%w: a rule ends by COUNT or by UNTIL, not both", ErrBadRecurrence)
	}
	opt, err := rrule.StrToROptionInLocation(strings.ToUpper(text), time.UTC)
	if err != nil {
		return bound{}, fmt.Errorf("%w: %q: %w", ErrBadRecurrence, clip(text), err)
	}
	switch {
	case opt.Freq == rrule.SECONDLY:
		return bound{}, fmt.Errorf("%w: FREQ=SECONDLY is not supported; the shortest cadence is "+
			"MINUTELY", ErrBadRecurrence)
	case opt.Count < 0 || opt.Count > maxCount:
		return bound{}, fmt.Errorf("%w: COUNT must be between 1 and %d", ErrBadRecurrence, maxCount)
	case opt.Interval < 0 || opt.Interval > maxInterval:
		return bound{}, fmt.Errorf("%w: INTERVAL must be between 1 and %d", ErrBadRecurrence,
			maxInterval)
	case seen["COUNT"] && opt.Count == 0:
		return bound{}, fmt.Errorf("%w: COUNT must be at least 1", ErrBadRecurrence)
	case seen["INTERVAL"] && opt.Interval == 0:
		return bound{}, fmt.Errorf("%w: INTERVAL must be at least 1", ErrBadRecurrence)
	}
	if n := byProduct(*opt); n > maxByProduct {
		return bound{}, fmt.Errorf("%w: its BY parts place %d occurrences in one period, more than "+
			"the %d this reads; name fewer values in BYSECOND, BYMINUTE, BYHOUR, BYMONTHDAY, and "+
			"BYMONTH", ErrBadRecurrence, n, maxByProduct)
	}
	b := bound{opt: *opt}
	if untilRaw != "" {
		wall, utc, err := parseWall(untilRaw)
		if err != nil {
			return bound{}, err
		}
		if utc {
			b.until = wall
		} else {
			// A floating UNTIL is read in the recurrence's zone, which is how AWX coerces one written
			// beside a DTSTART that names a zone.
			b.until = resolveWall(wall, rc.loc)
		}
	}
	b.opt.Until = time.Time{}
	b.opt.Dtstart = rc.start
	if _, err := rrule.NewRRule(b.opt); err != nil {
		return bound{}, fmt.Errorf("%w: %q: %w", ErrBadRecurrence, clip(text), err)
	}
	return b, nil
}

// parseDates reads an RDATE or EXDATE property into instants, each wall time placed in the zone its
// TZID names or, without one, the recurrence's zone.
func (rc *Recurrence) parseDates(field string) ([]time.Time, error) {
	params, value, ok := strings.Cut(field, ":")
	if !ok || value == "" {
		return nil, fmt.Errorf("%w: %q names no dates", ErrBadRecurrence, clip(field))
	}
	loc, named, err := rc.paramZone(params)
	if err != nil {
		return nil, err
	}
	if named == "" {
		loc = rc.loc
	}
	var out []time.Time
	for raw := range strings.SplitSeq(value, ",") {
		wall, utc, err := parseWall(strings.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		if !utc {
			placed, ok := placeWall(wall, loc, rc.gap)
			if !ok {
				continue
			}
			wall = placed
		}
		out = append(out, wall)
		if len(out) > maxDates {
			return nil, fmt.Errorf("%w: a recurrence may list at most %d dates", ErrBadRecurrence,
				maxDates)
		}
	}
	return out, nil
}

// byProduct returns how many occurrences a rule's BY parts place in one period, the product of the
// lengths of each BY list, counting an empty list as one. The library buffers this many time values
// per period before it yields the first, so a rule whose product is enormous exhausts memory on a
// document a few hundred bytes long.
func byProduct(opt rrule.ROption) int {
	lengths := []int{
		len(opt.Bysecond), len(opt.Byminute), len(opt.Byhour), len(opt.Bymonthday),
		len(opt.Byyearday), len(opt.Byweekno), len(opt.Bymonth), len(opt.Bysetpos),
		len(opt.Byweekday),
	}
	product := 1
	for _, n := range lengths {
		if n <= 1 {
			continue
		}
		// Stop multiplying once the product passes the bound, so a crafted rule cannot overflow the
		// count on its way to being refused.
		if product > maxByProduct/n+1 {
			return maxByProduct + 1
		}
		product *= n
	}
	return product
}

// parseWall reads an iCalendar DATE or DATE-TIME value as a zone-free wall clock held in UTC, and
// reports whether it carried the Z that makes it a real UTC instant.
func parseWall(value string) (time.Time, bool, error) {
	v := strings.ToUpper(strings.TrimSpace(value))
	utc := strings.HasSuffix(v, "Z")
	v = strings.TrimSuffix(v, "Z")
	layout := rrule.LocalDateTimeFormat
	if len(v) == len(rrule.DateFormat) {
		layout = rrule.DateFormat
	}
	t, err := time.ParseInLocation(layout, v, time.UTC)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%w: %q is not an iCalendar date or date-time such as "+
			"20260105T090000", ErrBadRecurrence, clip(value))
	}
	return t, utc, nil
}

// Next returns the first instant of the recurrence strictly after the given time, or ErrExhausted
// when a bounded rule has no fire left.
func (rc *Recurrence) Next(after time.Time) (time.Time, error) {
	after = after.Truncate(time.Second)
	lowOff, highOff := offsetRange(rc.loc, after)
	floor := utcReading(after).Add(time.Duration(lowOff) * time.Second)
	ex, err := rc.exclusions(floor, highOff)
	if err != nil {
		return time.Time{}, err
	}
	var best time.Time
	for _, b := range rc.include {
		found, err := rc.nextOf(b, after, floor, best, highOff, ex)
		if err != nil {
			return time.Time{}, err
		}
		if !found.IsZero() && (best.IsZero() || found.Before(best)) {
			best = found
		}
	}
	for _, d := range rc.rdates {
		if d.After(after) && (best.IsZero() || d.Before(best)) && !ex.has(d) {
			best = d
		}
	}
	if best.IsZero() {
		return time.Time{}, ErrExhausted
	}
	return best.In(rc.loc), nil
}

// nextOf returns the earlier of best and the first instant one rule generates after the given time
// that no exclusion removes, or best unchanged when the rule generates nothing earlier. floor is
// the earliest wall time that can be placed after the given time.
//
// Wall times are generated in order, but the instants they are placed at are not quite in order:
// a wall time inside a spring-forward gap is read with the offset before the gap and so lands later
// than the wall times just after the gap. The scan therefore keeps going past the first candidate
// until no later wall time could possibly land before it, which the zone's offset range decides.
func (rc *Recurrence) nextOf(b bound, after, floor, best time.Time, highOff int,
	ex *exclusionSet) (time.Time, error) {
	opt := b.opt
	fastForward(&opt, floor.Add(-fastForwardMargin))
	r, err := rrule.NewRRule(opt)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrBadRecurrence, err)
	}
	next := r.Iterator()
	examined := 0
	for {
		wall, ok := next()
		if !ok {
			return best, nil
		}
		// Every occurrence counts, including those at or before the asked-about time. A rule with an
		// ancient start and no fast-forward path grinds through all of them to reach the present, so
		// counting only the ones past the floor let it iterate without bound while the cap sat unused.
		examined++
		if examined > maxExamined {
			return time.Time{}, fmt.Errorf("%w: it generates more than the %d occurrences one "+
				"evaluation reads before reaching the next run, so it would be evaluated without end",
				ErrBadRecurrence, maxExamined)
		}
		if !wall.After(floor) {
			continue
		}
		// The earliest instant this wall time or any later one can be placed at.
		earliest := wall.Unix() - int64(highOff)
		if !best.IsZero() && earliest > best.Unix() {
			return best, nil
		}
		if !b.until.IsZero() && earliest > b.until.Unix() {
			return best, nil
		}
		at, placed := placeWall(wall, rc.loc, rc.gap)
		if !placed {
			continue
		}
		if !at.After(after) || (!b.until.IsZero() && at.After(b.until)) {
			continue
		}
		if !best.IsZero() && !at.Before(best) {
			continue
		}
		if ex.has(at) {
			continue
		}
		best = at
	}
}

// exclusionSet answers whether an EXDATE or EXRULE removes an instant, expanding each EXRULE once
// per evaluation rather than once per question.
//
// The questions arrive in nearly ascending order, so each EXRULE is expanded lazily up to the
// latest wall time a question can need and every instant it produced is remembered. Expanding a
// rule again for every candidate cost a full expansion per candidate, which for a rule that
// excludes most of what it generates turned one evaluation into minutes.
type exclusionSet struct {
	// loc is the zone wall times are placed in.
	loc *time.Location
	// highOff is the zone's highest offset, which bounds how late a wall time for an instant can be.
	highOff int
	// rules are the EXRULE expansions, each with its iterator and progress.
	rules []exclusionRule
	// hits are the excluded instants, in Unix seconds, from both EXDATE and the expansions so far.
	hits map[int64]bool
	// gap is the spring-forward setting wall times are placed by, the same one the rules use.
	gap string
}

// exclusionRule is one EXRULE being expanded.
type exclusionRule struct {
	// next yields the rule's wall times in order.
	next rrule.Next
	// until is the instant past which the rule excludes nothing, zero when unbounded.
	until time.Time
	// reached is the last wall time generated, so expansion resumes where it stopped.
	reached time.Time
	// done reports the rule has no more wall times.
	done bool
}

// exclusions builds the exclusion set for one evaluation whose candidates are all placed after
// floor.
func (rc *Recurrence) exclusions(floor time.Time, highOff int) (*exclusionSet, error) {
	ex := &exclusionSet{loc: rc.loc, highOff: highOff, hits: map[int64]bool{}, gap: rc.gap}
	for _, d := range rc.exdates {
		ex.hits[d.Unix()] = true
	}
	for _, b := range rc.exclude {
		opt := b.opt
		fastForward(&opt, floor.Add(-fastForwardMargin))
		r, err := rrule.NewRRule(opt)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadRecurrence, err)
		}
		ex.rules = append(ex.rules, exclusionRule{next: r.Iterator(), until: b.until})
	}
	return ex, nil
}

// has reports whether the instant is excluded. Any wall time placed at the instant is no later than
// its UTC reading plus the zone's highest offset, so each rule is expanded to that point and no
// further.
func (ex *exclusionSet) has(at time.Time) bool {
	upper := utcReading(at).Add(time.Duration(ex.highOff)*time.Second + time.Second)
	for i := range ex.rules {
		r := &ex.rules[i]
		for !r.done && !r.reached.After(upper) {
			wall, ok := r.next()
			if !ok {
				r.done = true
				break
			}
			r.reached = wall
			placed, ok := placeWall(wall, ex.loc, ex.gap)
			if ok && (r.until.IsZero() || !placed.After(r.until)) {
				ex.hits[placed.Unix()] = true
			}
		}
	}
	return ex.hits[at.Unix()]
}

// fastForward moves an unbounded sub-weekly rule's start forward by whole periods to just before
// target, so evaluating it costs the same whether it started yesterday or six years ago.
//
// A minutely rule that started years back otherwise iterates every minute since, which measured at
// most of a second for each schedule on each tick. Moving the start by a whole number of periods
// keeps every occurrence the rule generates after the new start exactly where it was: the period
// lattice, the defaults a rule takes from its start's minute, hour, and weekday, and the week
// alignment all survive the shift. A rule counted by COUNT is left alone, since moving its start
// would change which occurrences it counts, and monthly and yearly rules are cheap to iterate.
func fastForward(opt *rrule.ROption, target time.Time) {
	if opt.Count > 0 {
		return
	}
	var unit int64
	switch opt.Freq {
	case rrule.MINUTELY:
		unit = 60
	case rrule.HOURLY:
		unit = 3600
	case rrule.DAILY:
		unit = 86400
	case rrule.WEEKLY:
		unit = 7 * 86400
	default:
		return
	}
	interval := int64(max(opt.Interval, 1))
	period := unit * interval
	gap := target.Unix() - opt.Dtstart.Unix()
	if gap <= period {
		return
	}
	opt.Dtstart = opt.Dtstart.Add(time.Duration(gap/period*period) * time.Second)
}

// utcReading returns an instant's UTC clock as a zone-free wall reading, which is what a wall time
// placed at that instant is offset from.
func utcReading(t time.Time) time.Time {
	return time.Unix(t.Unix(), 0).UTC()
}

// zonePeriod is one stretch of time during which a zone keeps one offset.
type zonePeriod struct {
	// offset is the zone's offset east of UTC in seconds.
	offset int
	// start and end bound the stretch in Unix seconds, end exclusive.
	start, end int64
}

// zonePeriods returns the offset periods of loc covering the Unix seconds from and to, in order.
// The walk is bounded, since a real zone changes offset at most a few times in any span this asks
// about.
func zonePeriods(loc *time.Location, from, to int64) []zonePeriod {
	var out []zonePeriod
	t := time.Unix(from, 0).In(loc)
	for range 64 {
		_, off := t.Zone()
		begin, finish := t.ZoneBounds()
		p := zonePeriod{offset: off, start: math.MinInt64, end: math.MaxInt64}
		if !begin.IsZero() {
			p.start = begin.Unix()
		}
		if !finish.IsZero() {
			p.end = finish.Unix()
		}
		out = append(out, p)
		if finish.IsZero() || p.end > to {
			break
		}
		t = finish.In(loc)
	}
	return out
}

// offsetRange returns the lowest and highest offsets loc takes from shortly before after to a few
// years past it, which bounds how far a wall time and the instant it is placed at can differ.
func offsetRange(loc *time.Location, after time.Time) (int, int) {
	from := after.Unix() - 3*86400
	to := after.Unix() + 4*366*86400
	low, high := math.MaxInt, math.MinInt
	for _, p := range zonePeriods(loc, from, to) {
		low, high = min(low, p.offset), max(high, p.offset)
	}
	return low, high
}

// resolveWall places a zone-free wall clock in loc by the rules RFC 5545 gives for the two
// daylight-saving edges: a wall time that occurs twice is the first of the two, and one that does
// not occur is read with the offset in force before the gap, which lands it the length of the gap
// later. It is placeWall under the later setting, which never declines to place a time.
func resolveWall(wall time.Time, loc *time.Location) time.Time {
	at, _ := placeWall(wall, loc, SpringForwardLater)
	return at
}

// clip shortens caller-supplied text for an error message, so a refusal names the offending part
// without echoing a whole document back.
func clip(s string) string {
	const limit = 80
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..." + strconv.Itoa(len(s)-limit) + " more bytes"
}
