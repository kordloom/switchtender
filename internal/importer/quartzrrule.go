package importer

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// quartzMonths maps Quartz month names to their numbers.
var quartzMonths = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

// quartzDays maps Quartz weekday names to Quartz weekday numbers, Sunday being one.
var quartzDays = map[string]int{
	"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7,
}

// icalDays are the iCalendar weekday codes indexed by Quartz weekday number less one.
var icalDays = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

// rundeckQuartzFields returns a Rundeck schedule as the seven Quartz fields, seconds through year,
// from either the crontab or the structured form, and whether it is in Quartz form at all. A five
// field crontab is plain cron, which has none of the forms a recurrence is needed for.
func rundeckQuartzFields(sc *rundeckSchedule) ([]string, bool) {
	if spec := strings.TrimSpace(sc.Crontab); spec != "" {
		fields := strings.Fields(spec)
		switch len(fields) {
		case 6:
			return append(fields, "*"), true
		case 7:
			return fields, true
		default:
			return nil, false
		}
	}
	seconds := strings.TrimSpace(sc.Time.Seconds)
	if seconds == "" {
		seconds = "0"
	}
	return []string{
		seconds, rundeckField(sc.Time.Minute), rundeckField(sc.Time.Hour),
		rundeckField(sc.DayOfMonth.Day), rundeckField(sc.Month), rundeckField(sc.WeekDay.Day),
		rundeckField(sc.Year),
	}, true
}

// quartzNeedsRecurrence reports whether a Quartz expression uses a form cron has no reading for and
// an RFC 5545 recurrence does: the nth weekday of the month, the last given weekday, the last day
// of the month, or the last weekday of the month. These are the schedules that were refused before
// recurrences existed, which is why they and nothing else take this path.
func quartzNeedsRecurrence(fields []string) bool {
	dom, dow := strings.ToUpper(fields[3]), strings.ToUpper(fields[5])
	return strings.ContainsAny(dow, "#L") || strings.HasPrefix(dom, "L")
}

// quartzToRRule converts a seven-field Quartz expression into an RFC 5545 recurrence that fires at
// exactly the same moments, reporting whether it could. Its DTSTART is the import day at midnight,
// floating, so the rule reads in the schedule's zone the way the converted cron would.
//
// The forms it refuses are the ones a single rule cannot say exactly, chiefly the nearest-weekday
// form W, and a last weekday of the month asked for at more than one time of day, where BYSETPOS
// would pick only the last of those times. A refusal is silent here because the caller falls back
// to the cron conversion, which names the problem.
func quartzToRRule(fields []string, now time.Time) (string, bool) {
	seconds, ok := quartzList(fields[0], 0, 59, nil)
	if !ok {
		return "", false
	}
	minutes, ok := quartzList(fields[1], 0, 59, nil)
	if !ok {
		return "", false
	}
	hours, ok := quartzList(fields[2], 0, 23, nil)
	if !ok {
		return "", false
	}
	parts := []string{"FREQ=MONTHLY"}
	if month := fields[4]; month != "*" {
		months, ok := quartzList(month, 1, 12, quartzMonths)
		if !ok {
			return "", false
		}
		parts = append(parts, "BYMONTH="+joinInts(months))
	}
	dom, dow := strings.ToUpper(fields[3]), strings.ToUpper(fields[5])
	domSet, dowSet := dom != "?" && dom != "*", dow != "?" && dow != "*"
	if domSet && dowSet {
		return "", false
	}
	switch {
	case dom == "LW":
		if len(seconds)*len(minutes)*len(hours) != 1 {
			return "", false
		}
		parts = append(parts, "BYDAY=MO,TU,WE,TH,FR", "BYSETPOS=-1")
	case strings.HasPrefix(dom, "L"):
		back := 0
		if rest := strings.TrimPrefix(dom, "L"); rest != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(rest, "-"))
			if !strings.HasPrefix(rest, "-") || err != nil || n < 1 || n > 30 {
				return "", false
			}
			back = n
		}
		parts = append(parts, "BYMONTHDAY="+strconv.Itoa(-1-back))
	case domSet:
		days, ok := quartzList(dom, 1, 31, nil)
		if !ok {
			return "", false
		}
		parts = append(parts, "BYMONTHDAY="+joinInts(days))
	case dowSet:
		byday, ok := quartzWeekdays(dow)
		if !ok {
			return "", false
		}
		parts = append(parts, "BYDAY="+byday)
	}
	parts = append(parts, "BYHOUR="+joinInts(hours), "BYMINUTE="+joinInts(minutes),
		"BYSECOND="+joinInts(seconds))
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if year := fields[6]; year != "*" && year != "?" {
		first, last, ok := quartzYears(year)
		if !ok {
			return "", false
		}
		if begin := time.Date(first, 1, 1, 0, 0, 0, 0, time.UTC); begin.After(start) {
			start = begin
		}
		parts = append(parts, fmt.Sprintf("UNTIL=%04d1231T235959", last))
	}
	return "DTSTART:" + start.Format("20060102T150405") + "\nRRULE:" + strings.Join(parts, ";"),
		true
}

// quartzWeekdays converts a Quartz weekday field into an iCalendar BYDAY list: the nth weekday 6#3
// becomes 3FR, the last given weekday 6L becomes -1FR, and plain weekdays, ranges, and lists become
// their codes.
func quartzWeekdays(field string) (string, bool) {
	if day, nth, found := strings.Cut(field, "#"); found {
		d, ok := quartzDay(day)
		n, err := strconv.Atoi(nth)
		if !ok || err != nil || n < 1 || n > 5 {
			return "", false
		}
		return strconv.Itoa(n) + icalDays[d-1], true
	}
	if day, found := strings.CutSuffix(field, "L"); found {
		d, ok := quartzDay(day)
		if !ok {
			return "", false
		}
		return "-1" + icalDays[d-1], true
	}
	days, ok := quartzList(field, 1, 7, quartzDays)
	if !ok {
		return "", false
	}
	codes := make([]string, len(days))
	for i, d := range days {
		codes[i] = icalDays[d-1]
	}
	return strings.Join(codes, ","), true
}

// quartzDay reads one Quartz weekday, a number from one for Sunday or a three-letter name.
func quartzDay(s string) (int, bool) {
	if d, ok := quartzDays[s]; ok {
		return d, true
	}
	d, err := strconv.Atoi(s)
	if err != nil || d < 1 || d > 7 {
		return 0, false
	}
	return d, true
}

// quartzYears reads a Quartz year field of one year or one range into its first and last year.
func quartzYears(field string) (int, int, bool) {
	low, high, ranged := strings.Cut(field, "-")
	first, err := strconv.Atoi(low)
	if err != nil || first < 1970 || first > 2199 {
		return 0, 0, false
	}
	if !ranged {
		return first, first, true
	}
	last, err := strconv.Atoi(high)
	if err != nil || last < first || last > 2199 {
		return 0, 0, false
	}
	return first, last, true
}

// quartzList expands a Quartz field of values, ranges, steps, and lists into the sorted values it
// names, between low and high. names maps the field's three-letter names when it has them.
func quartzList(field string, low, high int, names map[string]int) ([]int, bool) {
	value := func(s string) (int, bool) {
		if n, ok := names[strings.ToUpper(s)]; ok {
			return n, true
		}
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= low && n <= high
	}
	var out []int
	for term := range strings.SplitSeq(field, ",") {
		span, stepText, stepped := strings.Cut(term, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return nil, false
			}
			step = n
		}
		from, to := low, high
		switch {
		case span == "*":
		case strings.Contains(span, "-"):
			a, b, _ := strings.Cut(span, "-")
			var okA, okB bool
			from, okA = value(a)
			to, okB = value(b)
			if !okA || !okB || to < from {
				return nil, false
			}
		default:
			n, ok := value(span)
			if !ok {
				return nil, false
			}
			from = n
			if !stepped {
				to = n
			}
		}
		for v := from; v <= to; v += step {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), len(out) > 0
}

// joinInts renders values as a comma separated list.
func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}
