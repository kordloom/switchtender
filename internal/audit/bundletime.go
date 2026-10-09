package audit

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// bundleTimeForm is the one time form a bundle may carry: a four-digit year, a two-digit month,
// day, hour, minute, and second, an upper case T between the date and the time, an optional
// fraction of one or more digits after a period, and an upper case Z. Every digit is ASCII.
var bundleTimeForm = regexp.MustCompile(
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$`)

// parseBundleTime reads a time a bundle carries in the one form the LoomSeal format allows: RFC
// 3339 narrowed to UTC, with a year from 0001 to 9999 and every field within its range, so a leap
// second is refused. time.Parse alone also reads a one-digit hour, a comma before the fraction, a
// numeric offset, and year 0000, each of which the reference verifier refuses, so a verdict built
// on it could pass a bundle the reference rejects. The result is whole microseconds, the digits
// past the microsecond dropped toward the earlier instant, the precision the format compares at.
func parseBundleTime(s string) (time.Time, error) {
	if !bundleTimeForm.MatchString(s) {
		return time.Time{}, fmt.Errorf("%q is not a UTC time of the form "+
			"YYYY-MM-DDTHH:MM:SS[.fraction]Z", s)
	}
	if strings.HasPrefix(s, "0000-") {
		return time.Time{}, fmt.Errorf("%q is in year 0000, and the first year allowed is 0001", s)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid time: %w", s, err)
	}
	return t.Truncate(time.Microsecond), nil
}

// readBundleTimes reads every time a bundle carries, refusing the bundle on the first one outside
// the one form and naming where it sits. It returns the claims' times in claim order, so every
// later check compares the times this read produced rather than parsing a claim's time again.
func readBundleTimes(b *Bundle) ([]time.Time, error) {
	if _, err := parseBundleTime(b.CreatedAt); err != nil {
		return nil, fmt.Errorf("%w: created_at: %w", ErrVerify, err)
	}
	at := make([]time.Time, len(b.Claims))
	for i := range b.Claims {
		t, err := parseBundleTime(b.Claims[i].At)
		if err != nil {
			return nil, fmt.Errorf("%w: claim %d at: %w", ErrVerify, i, err)
		}
		at[i] = t
	}
	for i := range b.Anchors {
		if _, err := parseBundleTime(b.Anchors[i].At); err != nil {
			return nil, fmt.Errorf("%w: anchor %d at: %w", ErrVerify, i, err)
		}
	}
	return at, nil
}
