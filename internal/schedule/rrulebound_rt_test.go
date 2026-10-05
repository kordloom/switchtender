package schedule

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// rtSeq returns start..start+n-1 joined by commas, for a BY list.
func rtSeq(start, n int) string {
	parts := make([]string, 0, n)
	for i := start; i < start+n; i++ {
		s, x := "", i
		if x == 0 {
			s = "0"
		}
		for x > 0 {
			s = string(byte('0'+x%10)) + s
			x /= 10
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

// TestParseRecurrenceBoundsByPartCardinality pins that a rule whose BY parts place an enormous number
// of occurrences in one period is refused at parse. The library buffers one whole period's
// cross-product before it yields a single occurrence, so FREQ=YEARLY naming every second, minute,
// hour, day, and month builds tens of millions of time values from a few hundred bytes and exhausts
// the process. A real schedule names a few BY values, so the bound does not reach it.
func TestParseRecurrenceBoundsByPartCardinality(t *testing.T) {
	t.Parallel()
	bomb := "DTSTART:20260101T000000Z\nRRULE:FREQ=YEARLY;BYMONTH=" + rtSeq(1, 12) + ";BYMONTHDAY=" + rtSeq(1, 28) +
		";BYHOUR=" + rtSeq(0, 24) + ";BYMINUTE=" + rtSeq(0, 60) + ";BYSECOND=" + rtSeq(0, 60)
	if _, err := ParseRecurrence(bomb, time.UTC); !errors.Is(err, ErrBadRecurrence) {
		t.Fatalf("ParseRecurrence(BY-part bomb) error = %v, want ErrBadRecurrence", err)
	}
	// Negative control: an ordinary rule with short BY lists parses and fires.
	ok := "DTSTART:20260101T000000Z\nRRULE:FREQ=DAILY;BYHOUR=2,14;BYMINUTE=0,30"
	rc, err := ParseRecurrence(ok, time.UTC)
	if err != nil {
		t.Fatalf("ParseRecurrence(ordinary rule) error = %v, want it parsed", err)
	}
	if _, err := rc.Next(time.Now()); err != nil {
		t.Errorf("Next() on an ordinary rule error = %v", err)
	}
}

// TestNextBoundsOccurrencesExamined pins that an evaluation that keeps generating occurrences without
// reaching the next run gives up rather than spinning. A rule whose exclusion rule removes every
// occurrence it generates would otherwise examine occurrences without end on every tick.
func TestNextBoundsOccurrencesExamined(t *testing.T) {
	t.Parallel()
	// Every minute is generated and every minute is excluded, so nextOf finds no next run and keeps
	// examining until the bound stops it.
	rule := "DTSTART:20200101T000000Z\nRRULE:FREQ=MINUTELY\nEXRULE:FREQ=MINUTELY"
	rc, err := ParseRecurrence(rule, time.UTC)
	if err != nil {
		t.Fatalf("ParseRecurrence() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := rc.Next(time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrBadRecurrence) {
			t.Fatalf("Next() error = %v, want ErrBadRecurrence once the examined bound is reached", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Next() did not give up on a rule whose every occurrence is excluded")
	}
}
