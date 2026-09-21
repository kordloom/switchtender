package run

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/event"
)

// TestFactsAreStampedWhenGatheredNotWhenSubmitted pins the fix for a quiet rewriting of history.
//
// A run's CreatedAt is its submission; a held run executes days later. Facts stamped with
// submission time landed in history buckets days in the past, some already swept by retention,
// so the estate's answer for "what was true last Tuesday" changed because somebody approved a
// run on Friday. A fact is gathered when the event carrying it happened, and that is its stamp.
func TestFactsAreStampedWhenGatheredNotWhenSubmitted(t *testing.T) {
	t.Parallel()
	submitted := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	executed := submitted.Add(72 * time.Hour)

	fold := NewSummaryFold(submitted)
	fold.Add([]event.Event{{
		Type: event.TypeFacts, Host: "db01", Time: executed,
		Facts: map[string]string{"os": "linux"},
	}})
	facts := fold.HostFacts()
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if !facts[0].GatheredAt.Equal(executed) {
		t.Errorf("GatheredAt = %s, want the execution instant %s: submission-stamped facts "+
			"rewrite estate history into buckets retention may already have swept",
			facts[0].GatheredAt, executed)
	}

	// An event with no time of its own still gets a stamp rather than a zero, so degenerate
	// input degrades to the old behavior instead of to an invisible bucket.
	fold2 := NewSummaryFold(submitted)
	fold2.Add([]event.Event{{Type: event.TypeFacts, Host: "db02",
		Facts: map[string]string{"os": "linux"}}})
	if got := fold2.HostFacts()[0].GatheredAt; !got.Equal(submitted) {
		t.Errorf("timeless event stamped %s, want the fold's own %s", got, submitted)
	}
}
