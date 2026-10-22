package witness

import (
	"fmt"
	"testing"
	"time"
)

// TestLimiterHoldsABurstThenOnePerInterval pins the push budget: sixty at once, the sixty-first
// refused with the wait until one is back, one more after each interval, and a refused request
// spending nothing.
func TestLimiterHoldsABurstThenOnePerInterval(t *testing.T) {
	t.Parallel()
	l := newLimiter(pushBurst, pushEvery)
	now := pushEpoch
	for i := 0; i < pushBurst; i++ {
		if ok, _ := l.allow("k", now); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	tests := []struct {
		// After is how long past the burst the request comes.
		After time.Duration
		// WantOK is whether it may proceed.
		WantOK bool
		// WantWait is the wait a refusal names.
		WantWait time.Duration
	}{{ // Test 0: The sixty-first at once is refused, one interval to wait.
		After: 0, WantWait: pushEvery,
	}, { // Test 1: Hammering does not push the recovery further away.
		After: time.Second, WantWait: pushEvery - time.Second,
	}, { // Test 2: One interval on, one request is back.
		After: pushEvery, WantOK: true,
	}, { // Test 3: And spent again.
		After: pushEvery, WantWait: pushEvery,
	}, { // Test 4: The next interval returns the next one.
		After: 2 * pushEvery, WantOK: true,
	}}
	for testNum, test := range tests {
		ok, wait := l.allow("k", now.Add(test.After))
		if ok != test.WantOK || wait != test.WantWait {
			t.Errorf("test %d: allow() = %v, %s, want %v, %s", testNum, ok, wait, test.WantOK,
				test.WantWait)
		}
	}
	// A whole budget is back after the burst's worth of intervals.
	later := now.Add(2*pushEvery + pushBurst*pushEvery)
	for i := 0; i < pushBurst; i++ {
		if ok, _ := l.allow("k", later); !ok {
			t.Fatalf("request %d of the refilled burst refused", i+1)
		}
	}
	if ok, _ := l.allow("k", later); ok {
		t.Error("the refilled budget held more than one burst")
	}
}

// TestLimiterKeysAreIndependent pins that one key spending its budget leaves every other key whole,
// so one install flooding the witness cannot spend another install's pushes.
func TestLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()
	l := newLimiter(2, time.Minute)
	for i := 0; i < 3; i++ {
		l.allow("busy", pushEpoch)
	}
	if ok, _ := l.allow("quiet", pushEpoch); !ok {
		t.Error("a key with a full budget was refused because another key spent its own")
	}
}

// TestLimiterForgetsWholeBudgets pins that the limiter does not grow without bound under a sweep of
// keys: past the prune bound, keys whose budget has refilled are forgotten, and keys still spending
// are kept.
func TestLimiterForgetsWholeBudgets(t *testing.T) {
	t.Parallel()
	l := newLimiter(1, time.Second)
	for i := 0; i <= limiterPrune; i++ {
		l.allow(fmt.Sprintf("k%d", i), pushEpoch)
	}
	l.allow("spending", pushEpoch.Add(time.Hour))
	l.allow("trigger", pushEpoch.Add(time.Hour))
	if n := len(l.full); n > 3 {
		t.Errorf("the limiter holds %d keys after the prune, want only the ones still spending", n)
	}
	if ok, _ := l.allow("spending", pushEpoch.Add(time.Hour)); ok {
		t.Error("the prune forgot a key that was still spending its budget")
	}
}

// TestLimiterPanicsOnANonsenseBudget pins that a limiter refusing everything or nothing is a
// programming error caught at construction.
func TestLimiterPanicsOnANonsenseBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Burst and Every are the budget.
		Burst int
		Every time.Duration
	}{{ // Test 0: No burst.
		Burst: 0, Every: time.Second,
	}, { // Test 1: No interval.
		Burst: 1, Every: 0,
	}, { // Test 2: A negative interval.
		Burst: 1, Every: -time.Second,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("newLimiter(%d, %s) did not panic", test.Burst, test.Every)
				}
			}()
			newLimiter(test.Burst, test.Every)
		})
	}
}
