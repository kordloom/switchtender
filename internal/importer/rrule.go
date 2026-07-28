package importer

import (
	"fmt"
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
	minute := firstOr(parts["BYMINUTE"], startMinute)
	hour := firstOr(parts["BYHOUR"], startHour)

	switch freq {
	case "MINUTELY":
		return everyField(interval) + " * * * *", true
	case "HOURLY":
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
		return fmt.Sprintf("%s %s %s * *", minute, hour, parts["BYMONTHDAY"]), true
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

// everyField returns a cron step field, collapsing an interval of one to a plain wildcard.
func everyField(interval string) string {
	if interval == "1" {
		return "*"
	}
	return "*/" + interval
}

// firstOr returns the first comma-separated value in s, or fallback when s is empty.
func firstOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	if before, _, ok := strings.Cut(s, ","); ok {
		return before
	}
	return s
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
