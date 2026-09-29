package importer

import (
	"fmt"
	"strconv"
	"strings"
)

// weekdayCron maps an iCalendar weekday code to its cron day-of-week number.
var weekdayCron = map[string]string{
	"SU": "0", "MO": "1", "TU": "2", "WE": "3", "TH": "4", "FR": "5", "SA": "6",
}

// RRULEToCron converts the common AWX schedule RRULE shapes into a standard five-field cron
// expression, reporting whether the conversion succeeded. Intervals a cron cannot express, such as
// every three days, are refused so the caller can warn rather than emit a wrong cadence.
func RRULEToCron(rrule string) (string, bool) {
	parts := parseRRULE(rrule)
	freq := parts["FREQ"]
	interval := parts["INTERVAL"]
	if interval == "" {
		interval = "1"
	}
	// AWX usually carries the time of day in DTSTART rather than BYHOUR and BYMINUTE, so fall
	// back to it. Defaulting straight to midnight would silently move a nightly job.
	startHour, startMinute := dtstartTime(rrule)
	// The values a rule supplies are held to a plain number before they are written into a cron
	// field. Pasting them through meant an export decided the cadence: a schedule named "Nightly at
	// 2am" carrying BYMINUTE=* and BYHOUR=* became "* * * * *", a run every minute, with no warning.
	// A rule this converter cannot express as a single number is refused so the caller can say so
	// rather than emit a cadence nobody asked for.
	minute, ok := numericField(parts["BYMINUTE"], startMinute, 0, 59)
	if !ok {
		return "", false
	}
	hour, ok := numericField(parts["BYHOUR"], startHour, 0, 23)
	if !ok {
		return "", false
	}

	switch freq {
	case "MINUTELY":
		// A step only lands on the cadence asked for when it divides its range evenly. "*/45" fires
		// at 0 and 45 past every hour, which is a 45 minute gap followed by a 15 minute one, not
		// every 45 minutes. The doc for this function already promised to refuse what cron cannot
		// express; that promise covered the daily and weekly arms only.
		if !dividesEvenly(interval, 60) {
			return "", false
		}
		return everyField(interval) + " * * * *", true
	case "HOURLY":
		if !dividesEvenly(interval, 24) {
			return "", false
		}
		return minute + " " + everyField(interval) + " * * *", true
	case "DAILY":
		if interval != "1" {
			return "", false
		}
		return fmt.Sprintf("%s %s * * *", minute, hour), true
	case "WEEKLY":
		if interval != "1" {
			return "", false
		}
		days, ok := cronDays(parts["BYDAY"])
		if !ok {
			return "", false
		}
		return fmt.Sprintf("%s %s * * %s", minute, hour, days), true
	case "MONTHLY":
		if interval != "1" || parts["BYMONTHDAY"] == "" {
			return "", false
		}
		day, dayOK := numericField(parts["BYMONTHDAY"], "", 1, 31)
		if !dayOK {
			return "", false
		}
		return fmt.Sprintf("%s %s %s * *", minute, hour, day), true
	default:
		return "", false
	}
}

// parseRRULE extracts the KEY=VALUE pairs from an iCalendar rule. AWX writes DTSTART and RRULE on
// separate lines in some exports and on one space-separated line in others, so both are accepted
// and the DTSTART part is skipped here.
func parseRRULE(rrule string) map[string]string {
	out := map[string]string{}
	for field := range strings.FieldsSeq(strings.ReplaceAll(rrule, "\n", " ")) {
		rule, ok := strings.CutPrefix(field, "RRULE:")
		if !ok {
			continue
		}
		for kv := range strings.SplitSeq(rule, ";") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				out[strings.ToUpper(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

// dtstartTime reads the hour and minute out of an iCalendar DTSTART, in either the plain or the
// TZID-qualified form. It returns midnight when there is no usable DTSTART, which matches the
// iCalendar default. The wall-clock time is used as written, since cron fires in the server's
// local time and that is the time the operator chose.
func dtstartTime(rrule string) (hour, minute string) {
	for field := range strings.FieldsSeq(strings.ReplaceAll(rrule, "\n", " ")) {
		if !strings.HasPrefix(strings.ToUpper(field), "DTSTART") {
			continue
		}
		// The value follows the last colon: DTSTART:20260101T020000Z or
		// DTSTART;TZID=America/New_York:20260101T020000.
		idx := strings.LastIndex(field, ":")
		if idx < 0 {
			continue
		}
		value := field[idx+1:]
		t := strings.IndexAny(value, "Tt")
		if t < 0 || len(value) < t+5 {
			continue
		}
		clock := value[t+1:]
		if len(clock) < 4 {
			continue
		}
		h, m := clock[0:2], clock[2:4]
		if !isTwoDigits(h) || !isTwoDigits(m) {
			continue
		}
		return trimZero(h), trimZero(m)
	}
	return "0", "0"
}

// isTwoDigits reports whether s is exactly two ASCII digits.
func isTwoDigits(s string) bool {
	if len(s) != 2 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// trimZero drops a leading zero from a two-digit clock field so the cron field reads 2 rather
// than 02, keeping a single zero for midnight.
func trimZero(s string) string {
	trimmed := strings.TrimLeft(s, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

// numericField returns value as a single cron number within low and high, falling back to fallback
// when value is empty. Anything else, including a list, a range, a star, or a step, is refused: a
// converter that pastes those through lets the file being imported choose the cadence.
func numericField(value, fallback string, low, high int) (string, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		v = fallback
	}
	if v == "" {
		return "", false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < low || n > high {
		return "", false
	}
	return strconv.Itoa(n), true
}

// dividesEvenly reports whether a step interval lands on the same offsets every cycle of size span.
// A step that does not divide its range produces an uneven cadence rather than the one requested.
func dividesEvenly(interval string, span int) bool {
	n, err := strconv.Atoi(strings.TrimSpace(interval))
	if err != nil || n < 1 || n > span {
		return false
	}
	return span%n == 0
}

// everyField returns a cron step field, collapsing an interval of one to a plain wildcard.
func everyField(interval string) string {
	if interval == "1" {
		return "*"
	}
	return "*/" + interval
}

// cronDays converts an RRULE BYDAY list into a cron day-of-week list, reporting whether every code
// was recognized.
func cronDays(byday string) (string, bool) {
	if byday == "" {
		return "*", true
	}
	var days []string
	for code := range strings.SplitSeq(byday, ",") {
		n, ok := weekdayCron[strings.ToUpper(strings.TrimSpace(code))]
		if !ok {
			return "", false
		}
		days = append(days, n)
	}
	return strings.Join(days, ","), true
}
