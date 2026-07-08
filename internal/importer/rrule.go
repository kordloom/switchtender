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
	minute := firstOr(parts["BYMINUTE"], "0")
	hour := firstOr(parts["BYHOUR"], "0")

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

// parseRRULE extracts the KEY=VALUE pairs from the RRULE line of an iCalendar rule, ignoring the
// DTSTART line AWX prepends.
func parseRRULE(rrule string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(rrule, "\n") {
		line = strings.TrimSpace(line)
		rule, ok := strings.CutPrefix(line, "RRULE:")
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
