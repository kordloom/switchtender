package witness

import (
	"sync"
	"time"
)

// Pushes are limited per install to a burst of pushBurst and one every pushEvery after it, which no
// honest beat cadence comes near and which keeps one install from spending the witness on another's
// behalf. Enrollments are limited per license the same way, more tightly, since an install enrolls
// once and every attempt costs a license verification.
const (
	// pushBurst is how many pushes one install may make at once.
	pushBurst = 60
	// pushEvery is the sustained interval between pushes once the burst is spent.
	pushEvery = 10 * time.Second
	// enrollBurst is how many enrollments one license may make at once.
	enrollBurst = 10
	// enrollEvery is the sustained interval between enrollments once the burst is spent.
	enrollEvery = time.Minute
	// limiterPrune is the key count past which keys whose budget has fully refilled are forgotten,
	// so a sweep of keys cannot grow the limiter without bound.
	limiterPrune = 4096
)

// limiter is a token bucket per key: a key may spend burst requests at once, and one more every
// interval after that. It keeps one instant per key, the time at which the key's budget is full
// again, so a check is a comparison and an update rather than a refill computed over a counter. It
// is the same idea as the server's sign-in limiter, kept here because this package cannot import
// the server's internal packages.
type limiter struct {
	// mu guards full.
	mu sync.Mutex
	// full maps a key to the instant its budget is whole again. A key not present is whole now.
	full map[string]time.Time
	// burst is how many requests a whole budget holds.
	burst int
	// every is how long one spent request takes to come back.
	every time.Duration
}

// newLimiter returns a limiter holding burst requests per key and returning one every interval. It
// panics on a burst under one or a non-positive interval, which would refuse everything or nothing.
func newLimiter(burst int, every time.Duration) *limiter {
	if burst < 1 || every <= 0 {
		panic("witness: a limiter needs a positive burst and interval")
	}
	return &limiter{full: map[string]time.Time{}, burst: burst, every: every}
}

// allow spends one request of key's budget at now. It reports whether the request may proceed and,
// when it may not, how long until it could. A refused request spends nothing, so a caller hammering
// a spent budget does not push its own recovery further away.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.full) > limiterPrune {
		for k, at := range l.full {
			if !at.After(now) {
				delete(l.full, k)
			}
		}
	}
	full, ok := l.full[key]
	if !ok || full.Before(now) {
		full = now
	}
	// The budget is spent when the instant it is whole again lies more than burst-1 intervals ahead:
	// burst requests at one instant move it burst intervals ahead, and the next one is refused.
	room := time.Duration(l.burst-1) * l.every
	if ahead := full.Sub(now); ahead > room {
		return false, ahead - room
	}
	l.full[key] = full.Add(l.every)
	return true, 0
}
