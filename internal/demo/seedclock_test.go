package demo

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestNewSeedClockOpensInThePastAndStaysThere pins where the demo's clock starts and where it stops.
//
// Every seeded run is stamped from this clock, so a cursor that opened at or after the real present
// would put run history in the future, and a ceiling that reached the present would put a seeded run
// at the same instant a visitor loads the page. The window it opens is what makes the activity chart
// and the runs list read like a fleet that has been working rather than one created a second ago.
func TestNewSeedClockOpensInThePastAndStaysThere(t *testing.T) {
	t.Parallel()
	before := time.Now()
	c := NewSeedClock()
	after := time.Now()

	if c.cursor.Before(before.Add(-seedRunWindow)) || c.cursor.After(after.Add(-seedRunWindow)) {
		t.Errorf("cursor = %v, want about %v ago", c.cursor, seedRunWindow)
	}
	if c.ceiling.After(after.Add(-seedRunMargin)) {
		t.Errorf("ceiling = %v, want no later than %v before now", c.ceiling, seedRunMargin)
	}
	if !c.cursor.Before(c.ceiling) {
		t.Errorf("cursor %v is not before ceiling %v, so the clock has no room to advance",
			c.cursor, c.ceiling)
	}
	got := c.Now()
	if !got.Before(time.Now().Add(-seedRunMargin + time.Second)) {
		t.Errorf("Now() = %v, which is not safely in the past", got)
	}
}

// TestSeedClockAdvancesAndNeverInverts pins the ordering guarantee the run timeline depends on.
//
// A run's created, claimed, started, and ended stamps all come from this clock, so a reading that
// repeated or went backward would produce a run that ended before it began. Every reading has to
// move forward by at least a millisecond even when no real time passed between two calls, which is
// what keeps two stamps taken in the same instant distinguishable.
func TestSeedClockAdvancesAndNeverInverts(t *testing.T) {
	t.Parallel()
	c := NewSeedClock()
	prev := c.cursor
	for i := range 50 {
		got := c.Now()
		if !got.After(prev) {
			t.Fatalf("read %d: Now() = %v, which is not after the previous reading %v", i, got, prev)
		}
		if got.Sub(prev) < time.Millisecond {
			t.Errorf("read %d: step = %v, want at least a millisecond", i, got.Sub(prev))
		}
		if got.After(c.ceiling) {
			t.Fatalf("read %d: Now() = %v, past the ceiling %v", i, got, c.ceiling)
		}
		prev = got
	}
}

// TestSeedClockAdvanceStepsTheWindowForward pins the between-run step. The seeder calls it once each
// run has landed so the next run's stamps open a fresh window, which is what spreads the seeded
// history across hours instead of piling it into one minute.
func TestSeedClockAdvanceStepsTheWindowForward(t *testing.T) {
	t.Parallel()
	c := NewSeedClock()
	start := c.Now()
	c.advance(seedRunGap)
	next := c.Now()
	if next.Sub(start) < seedRunGap {
		t.Errorf("after advance(%v) the next reading moved %v, want at least the gap",
			seedRunGap, next.Sub(start))
	}
}

// TestSeedClockClampsAtItsCeiling pins the upper bound. The ceiling exists so no seeded stamp lands
// near the present, and it has to hold however many readings and steps the seeder makes, including
// a step far larger than the room left.
func TestSeedClockClampsAtItsCeiling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels how the clock is pushed at its ceiling.
		Name string
		// Push drives the clock past where its ceiling sits.
		Push func(c *SeedClock)
	}{{ // Test 0: Repeated readings cannot pass the ceiling.
		Name: "reads",
		Push: func(c *SeedClock) {
			for range 20 {
				c.Now()
			}
		},
	}, { // Test 1: A single oversized step cannot pass it either.
		Name: "one huge step",
		Push: func(c *SeedClock) { c.advance(100 * time.Hour) },
	}, { // Test 2: Repeated steps cannot accumulate past it.
		Name: "many steps",
		Push: func(c *SeedClock) {
			for range 20 {
				c.advance(seedRunGap)
			}
		},
	}, { // Test 3: Steps and reads mixed together.
		Name: "steps and reads",
		Push: func(c *SeedClock) {
			for range 20 {
				c.advance(seedRunGap)
				c.Now()
			}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			base := time.Now().Add(-time.Hour)
			c := &SeedClock{cursor: base, realAt: time.Now(), ceiling: base.Add(5 * time.Millisecond)}
			test.Push(c)
			if c.cursor.After(c.ceiling) {
				t.Errorf("%s: cursor = %v, past the ceiling %v", test.Name, c.cursor, c.ceiling)
			}
			if got := c.Now(); got.After(c.ceiling) {
				t.Errorf("%s: Now() = %v, past the ceiling %v", test.Name, got, c.ceiling)
			}
		})
	}
}

// TestSeedClockSurvivesABackwardWallClock pins the floor on each step. The clock advances by the
// real time elapsed since the last reading, so a wall clock that moved backward between two reads
// would otherwise produce a negative step and invert the run's stamps.
func TestSeedClockSurvivesABackwardWallClock(t *testing.T) {
	t.Parallel()
	base := time.Now().Add(-2 * time.Hour)
	// realAt in the future is what a backward jump of the host clock looks like from here.
	c := &SeedClock{cursor: base, realAt: time.Now().Add(time.Hour), ceiling: time.Now().Add(-time.Minute)}
	prev := c.cursor
	for i := range 5 {
		got := c.Now()
		if !got.After(prev) {
			t.Fatalf("read %d: Now() = %v, not after %v, so a backward host clock inverted the run",
				i, got, prev)
		}
		prev = got
	}
}

// TestSeedClockIsSafeForConcurrentUse drives the clock from many goroutines the way the dispatcher's
// workers and the seeder do at once. The clock is handed to dispatch.WithClock and read from every
// shard execution, so a race here is a corrupted timeline on the one install strangers try first.
// Run with -race for this to mean anything.
func TestSeedClockIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	c := NewSeedClock()
	const workers = 16
	const reads = 40

	var mu sync.Mutex
	seen := make([]time.Time, 0, workers*reads)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range reads {
				got := c.Now()
				mu.Lock()
				seen = append(seen, got)
				mu.Unlock()
				if i%10 == 0 {
					c.advance(time.Second)
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != workers*reads {
		t.Fatalf("collected %d readings, want %d", len(seen), workers*reads)
	}
	ceiling := c.ceiling
	for i, got := range seen {
		if got.After(ceiling) {
			t.Fatalf("reading %d = %v, past the ceiling %v", i, got, ceiling)
		}
		if got.After(time.Now()) {
			t.Fatalf("reading %d = %v, which is in the future", i, got)
		}
	}

	// Every reading is distinct or later than one taken before it, which is what stops two runs
	// sharing a stamp however the goroutines interleaved.
	unique := make(map[time.Time]bool, len(seen))
	for _, got := range seen {
		unique[got] = true
	}
	if len(unique) < 2 {
		t.Errorf("the clock handed out %d distinct times across %d reads", len(unique), len(seen))
	}
}
