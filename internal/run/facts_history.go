package run

import (
	"sync/atomic"
	"time"
)

// DefaultFactsInterval is the minimum spacing between retained host state snapshots when none is
// configured. A day is chosen because the question this history answers is "what did the estate
// look like on the audit date", never "what did it look like at 14:32:07", and a fact set runs
// hundreds of kilobytes per host: keeping one gather per host per day rather than every gather is
// roughly two orders of magnitude less disk for an answer of the same quality.
const DefaultFactsInterval = 24 * time.Hour

// factsInterval holds the configured spacing. Atomic because the sweeper and the run executors read
// it from different goroutines while startup writes it once.
var factsInterval atomic.Int64

// SetFactsInterval configures the minimum spacing between retained host state snapshots. Zero keeps
// every gather, which is the opt-in for full depth. It is set once at startup.
func SetFactsInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	factsInterval.Store(int64(d))
	factsIntervalSet.Store(true)
}

// FactsInterval reports the configured spacing, defaulting when startup never set one.
func FactsInterval() time.Duration {
	raw := factsInterval.Load()
	if raw == 0 && !factsIntervalSet.Load() {
		return DefaultFactsInterval
	}
	return time.Duration(raw)
}

// factsIntervalSet distinguishes "never configured", which takes the default, from "configured to
// zero", which is the deliberate request to keep every gather. Without it those two are the same
// stored value and the opt-in for full depth would silently become the daily default.
var factsIntervalSet atomic.Bool

// FactsBucket names the slot a host's facts occupy in its state history.
//
// Rows are keyed by host and bucket, so a second gather landing in the same bucket replaces the
// first and the newest reading of each period survives. With an interval the bucket is the gather
// time truncated to it; without one the bucket is the run id, which is unique per gather and so
// keeps them all.
//
// Changing the interval on a running install does not corrupt anything. Rows already written keep
// the buckets they were written with, and only new rows use the new spacing, so the history reads
// as a mix of granularities rather than as a gap.
func FactsBucket(at time.Time, runID string, interval time.Duration) string {
	if interval <= 0 {
		return runID
	}
	return at.UTC().Truncate(interval).Format(time.RFC3339)
}

// DefaultFactsDepth is how many host state snapshots are kept per host when none is configured.
// At the default daily spacing that is a little over a year, which covers an annual audit and its
// predecessor. It is bounded by default rather than unbounded, unlike the summary tables: a fact
// set runs hundreds of kilobytes, so an unbounded default would quietly turn a large fleet's disk
// into a problem the operator did not choose.
const DefaultFactsDepth = 400

// factsDepth holds the configured cap, with the same set-flag treatment as the interval so that a
// deliberate zero, meaning keep everything, is distinguishable from never having been configured.
var (
	factsDepth    atomic.Int64
	factsDepthSet atomic.Bool
)

// SetFactsDepth configures how many host state snapshots survive per host. Zero keeps every
// snapshot, which is the opt-in for unbounded depth. It is set once at startup.
func SetFactsDepth(n int) {
	if n < 0 {
		n = 0
	}
	factsDepth.Store(int64(n))
	factsDepthSet.Store(true)
}

// FactsDepth reports the configured cap, defaulting when startup never set one.
func FactsDepth() int {
	raw := factsDepth.Load()
	if raw == 0 && !factsDepthSet.Load() {
		return DefaultFactsDepth
	}
	return int(raw)
}
