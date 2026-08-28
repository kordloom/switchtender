package server

import (
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// runHistograms accumulates run timings into cumulative counters that only ever climb.
//
// These series were recomputed from scratch on every scrape, over the newest metricsHistogramWindow
// runs. Prometheus reads a histogram's buckets, sum, and count as counters: a value that falls is
// not a smaller number, it is a counter reset, and the whole prior total is added back in. While an
// install held fewer runs than the window that never came up, because the window held everything.
// Past it, each scrape dropped the oldest runs out of one end, so any bucket could fall between two
// scrapes and every fall was read as a reset. The rates and quantiles built on these did not
// degrade gracefully at that point, they invented traffic that never happened, and they did it
// first on the installs with the most history, which are the ones watching these dashboards.
//
// Folding each run in once, and only once, is what a client library does for an in-process
// histogram, and it gives the same guarantee: counters climb for as long as the process lives, and
// a restart is an honest reset Prometheus already knows how to read. The cost is a scrape cheaper
// than the one it replaces, since only runs that finished since the last scrape are folded in.
type runHistograms struct {
	mu sync.Mutex
	// duration and queue hold the cumulative bucket counts, running in step with their bounds.
	duration, queue histogramCounts
	// mark is the end instant of the newest run folded in, and marked names the runs sharing that
	// instant. Run ids are compared at the boundary because two runs can end in the same
	// nanosecond, and a watermark alone would have to either double count them or drop one.
	mark    time.Time
	marked  map[string]bool
	started bool
	// behind counts scrapes where the window may not have held every newly finished run, so an
	// operator can see the exporter fell behind rather than reading undercounted series as calm.
	behind int
}

// histogramCounts is one cumulative histogram: a count per bucket, the running sum, and the
// number of observations.
type histogramCounts struct {
	// counts holds one cumulative count per bucket bound, in the bounds' own order.
	counts []int
	// sum is the total of every observation folded in.
	sum float64
	// total is how many observations were folded in.
	total int
}

// observe folds one value into every bucket whose bound it falls within.
func (h *histogramCounts) observe(bounds []float64, v float64) {
	if h.counts == nil {
		h.counts = make([]int, len(bounds))
	}
	h.total++
	h.sum += v
	for i, le := range bounds {
		if v <= le {
			h.counts[i]++
		}
	}
}

// newRunHistograms returns an accumulator with no runs folded in yet.
func newRunHistograms() *runHistograms {
	return &runHistograms{marked: map[string]bool{}}
}

// fold takes the newest page of run timings and counts every terminal run it has not counted before.
//
// The page arrives newest first and is walked oldest first, so the watermark only ever moves
// forward. A run is folded when it reaches a terminal state, which is also when its queue wait
// stops changing, so both histograms are fed from the same instant and neither can count a run
// twice.
func (a *runHistograms) fold(runs []run.RunTiming, windowFull bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// A page that is full and holds nothing older than the watermark cannot prove it held every run
	// that finished since the last scrape, so the gap is recorded rather than passed off as zero.
	if windowFull && a.started && !a.pageReachesMark(runs) {
		a.behind++
	}

	for i := len(runs) - 1; i >= 0; i-- {
		rn := runs[i]
		if rn.EndedAt == nil || rn.StartedAt == nil {
			continue
		}
		if !a.claim(rn) {
			continue
		}
		if d := rn.EndedAt.Sub(*rn.StartedAt).Seconds(); d >= 0 {
			a.duration.observe(durationBuckets, d)
		}
		if q := rn.StartedAt.Sub(rn.CreatedAt).Seconds(); q >= 0 {
			a.queue.observe(queueBuckets, q)
		}
	}
}

// pageReachesMark reports whether the page extends back to runs already folded in, which is what
// makes it able to prove nothing finished in between and went uncounted.
func (a *runHistograms) pageReachesMark(runs []run.RunTiming) bool {
	for _, rn := range runs {
		if rn.EndedAt != nil && !rn.EndedAt.After(a.mark) {
			return true
		}
	}
	return false
}

// claim reports whether this run is new to the accumulator, and records it when it is.
func (a *runHistograms) claim(rn run.RunTiming) bool {
	end := *rn.EndedAt
	switch {
	case !a.started || end.After(a.mark):
		a.mark, a.started = end, true
		a.marked = map[string]bool{rn.ID: true}
		return true
	case end.Equal(a.mark):
		if a.marked[rn.ID] {
			return false
		}
		a.marked[rn.ID] = true
		return true
	default:
		return false
	}
}

// snapshot returns the counts to emit, copied so the caller can format them without the lock.
func (a *runHistograms) snapshot() (duration, queue histogramCounts, behind int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.duration.clone(len(durationBuckets)), a.queue.clone(len(queueBuckets)), a.behind
}

// clone copies the counts, filling in the bucket slice when nothing has been folded in yet.
func (h histogramCounts) clone(n int) histogramCounts {
	out := histogramCounts{counts: make([]int, n), sum: h.sum, total: h.total}
	copy(out.counts, h.counts)
	return out
}
