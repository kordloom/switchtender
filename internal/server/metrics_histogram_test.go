package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// timing builds one terminal run timing: submitted at base, started after wait, ended after dur.
func timing(id string, base time.Time, wait, dur time.Duration) run.RunTiming {
	started := base.Add(wait)
	ended := started.Add(dur)
	return run.RunTiming{
		ID: id, Status: run.StatusSucceeded, CreatedAt: base, StartedAt: &started, EndedAt: &ended,
	}
}

// TestRunHistogramsOnlyClimb pins that the accumulated histograms never fall as runs age out of the
// page a scrape reads.
//
// Prometheus reads a histogram's buckets, sum, and count as counters: a value that falls is a
// counter reset, not a smaller number, and the whole prior total is added back in. These were
// recomputed from the newest page on every scrape, so once an install held more runs than the page,
// each scrape dropped the oldest out of one end and any bucket could fall. The rates and quantiles
// built on them did not degrade at that point, they invented traffic, and they did it first on the
// installs with the longest history.
func TestRunHistogramsOnlyClimb(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	// Fifty runs in the order history makes them, oldest first, one a minute.
	history := make([]run.RunTiming, 0, 50)
	for i := 0; i < 50; i++ {
		history = append(history, timing(fmt.Sprintf("run_%02d", i),
			base.Add(time.Duration(i)*time.Minute), 2*time.Second, 3*time.Second))
	}
	// page returns what a store returns to a scrape: the newest ten of the runs that exist by then,
	// newest first. Older runs fall off the end as history grows, which is the shape that made the
	// recomputed histograms fall.
	page := func(existing int) []run.RunTiming {
		out := make([]run.RunTiming, 0, 10)
		for i := existing - 1; i >= 0 && len(out) < 10; i-- {
			out = append(out, history[i])
		}
		return out
	}

	a := newRunHistograms()
	var prevDur, prevQueue histogramCounts
	for scrape, existing := 0, 10; existing <= 50; scrape, existing = scrape+1, existing+5 {
		a.fold(page(existing), false)
		dur, queue, _ := a.snapshot()
		assertClimbs(t, scrape, "duration", prevDur, dur)
		assertClimbs(t, scrape, "queue", prevQueue, queue)
		prevDur, prevQueue = dur, queue
	}

	dur, _, _ := a.snapshot()
	if dur.total != 50 {
		t.Errorf("count = %d, want every one of the 50 runs folded in exactly once", dur.total)
	}
}

// assertClimbs fails when any part of a cumulative histogram fell between two scrapes.
func assertClimbs(t *testing.T, scrape int, name string, prev, now histogramCounts) {
	t.Helper()
	if now.total < prev.total {
		t.Errorf("scrape %d: %s count fell %d to %d, which Prometheus reads as a counter reset and "+
			"credits as a burst of traffic that never happened", scrape, name, prev.total, now.total)
	}
	if now.sum < prev.sum {
		t.Errorf("scrape %d: %s sum fell %g to %g", scrape, name, prev.sum, now.sum)
	}
	for i := range now.counts {
		if i < len(prev.counts) && now.counts[i] < prev.counts[i] {
			t.Errorf("scrape %d: %s bucket %d fell %d to %d", scrape, name, i,
				prev.counts[i], now.counts[i])
		}
	}
}

// TestRunHistogramsCountEachRunOnce pins that a run read by many scrapes is folded in once, and that
// two runs sharing an end instant are told apart rather than collapsed.
func TestRunHistogramsCountEachRunOnce(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	// Two runs that end in the very same nanosecond. Only their ids separate them.
	page := []run.RunTiming{
		timing("run_b", base, time.Second, 4*time.Second),
		timing("run_a", base, time.Second, 4*time.Second),
	}
	a := newRunHistograms()
	for i := 0; i < 5; i++ {
		a.fold(page, false)
	}
	dur, queue, _ := a.snapshot()
	if dur.total != 2 {
		t.Errorf("count = %d, want 2: five scrapes over the same page must fold each run once, and "+
			"two runs that end in the same instant are still two runs", dur.total)
	}
	if queue.total != 2 {
		t.Errorf("queue count = %d, want 2", queue.total)
	}
	if want := 2 * 4.0; dur.sum != want {
		t.Errorf("sum = %g, want %g", dur.sum, want)
	}
}

// TestRunHistogramsReportFallingBehind pins that a scrape which cannot prove it saw every newly
// finished run says so, rather than passing undercounted series off as calm.
func TestRunHistogramsReportFallingBehind(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	a := newRunHistograms()
	a.fold([]run.RunTiming{timing("run_0", base, time.Second, time.Second)}, true)
	if _, _, behind := a.snapshot(); behind != 0 {
		t.Errorf("behind = %d, want 0 on the first scrape, which has nothing to fall behind", behind)
	}
	// A full page holding nothing the accumulator has already seen cannot prove the gap is empty.
	a.fold([]run.RunTiming{timing("run_9", base.Add(time.Hour), time.Second, time.Second)}, true)
	if _, _, behind := a.snapshot(); behind != 1 {
		t.Errorf("behind = %d, want 1: a full page that does not reach back to the last run folded "+
			"in may have skipped runs, and silence there reads as calm", behind)
	}
}

// TestMetricsHistogramSurvivesAPurge pins the same guarantee through the handler: a run that leaves
// the store no longer appears in a scrape's page, and its observation still must not be withdrawn.
func TestMetricsHistogramSurvivesAPurge(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	started, ended := base, base.Add(45*time.Second)
	if err := store.Save(ctx, &run.Run{
		ID: "run_ok", Status: run.StatusSucceeded, CreatedAt: base,
		StartedAt: &started, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	handler := metricsHandler(store, nil, nil, zap.NewNop())
	scrape := func() string {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}
	if body := scrape(); !strings.Contains(body, "switchtender_run_duration_seconds_count 1") {
		t.Fatalf("the run was not counted:\n%s", body)
	}
	if _, err := store.PurgeRunsBefore(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if body := scrape(); !strings.Contains(body, "switchtender_run_duration_seconds_count 1") {
		t.Errorf("the count was withdrawn when the run left the store: a histogram that falls is "+
			"read as a counter reset, not as a smaller number\n%s", body)
	}
}
