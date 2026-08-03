package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/spanbeat"
)

// durationBuckets are the upper bounds, in seconds, of the run-duration histogram. They span a quick
// command to a long playbook so both ends of the fleet's runtime show up.
var durationBuckets = []float64{1, 5, 10, 30, 60, 300, 600, 1800, 3600}

// queueBuckets are the upper bounds, in seconds, of the queue-wait histogram. They are tighter than the
// run-duration bounds since a healthy pool drains its backlog in seconds, not the minutes a run itself
// takes, so a rising queue wait is the early signal that the pool is undersized.
var queueBuckets = []float64{0.5, 1, 2, 5, 10, 30, 60, 300, 1800}

// metricsHistogramWindow caps how many recent runs feed the duration and queue-wait histograms,
// so a scrape reads a bounded page instead of the whole run history.
const metricsHistogramWindow = 10000

// metricsHandler serves run, fleet, queue, worker, and run-duration series in the Prometheus text
// exposition format, computed from the store at scrape time so no counter state lives in the
// process. The status gauges come from a grouped count, and the histograms are derived from the
// most recent metricsHistogramWindow runs, so a scrape stays cheap however large history grows.
func metricsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: metricsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		byStatus, err := store.RunStatusCounts(r.Context())
		if err != nil {
			log.Error("server: metrics: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute metrics")
			return
		}
		runs, err := store.ListPage(r.Context(), run.ListFilter{}, metricsHistogramWindow, 0)
		if err != nil {
			log.Error("server: metrics: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute metrics")
			return
		}

		var b strings.Builder
		b.WriteString("# HELP switchtender_runs_total Top level runs by status.\n")
		b.WriteString("# TYPE switchtender_runs_total gauge\n")
		for _, status := range []run.Status{
			run.StatusPending, run.StatusRunning, run.StatusSucceeded,
			run.StatusFailed, run.StatusCanceled, run.StatusInterrupted,
		} {
			fmt.Fprintf(&b, "switchtender_runs_total{status=%q} %d\n", status, byStatus[status])
		}

		writeQueueDepth(&b, store, r)
		writeFleetHealth(&b, store, r)
		writeWorkers(&b, store, r)
		writeRunDurations(&b, runs)
		writeQueueWait(&b, runs)
		writeSpanBeats(&b, time.Now())

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}

// writeQueueDepth emits the count of pending unclaimed executable runs waiting per queue, the backlog
// a scaler watches. It reads non-terminal runs so shard and pipeline children in the queue count too.
// The default queue is always emitted so its series exists even when the backlog is empty.
func writeQueueDepth(b *strings.Builder, store run.Store, r *http.Request) {
	pending, err := store.NonTerminal(r.Context())
	if err != nil {
		return
	}
	depth := map[string]int{"": 0}
	for _, rn := range pending {
		if rn.Status == run.StatusPending && rn.ClaimedBy == "" && rn.Kind == "" {
			depth[rn.Queue]++
		}
	}
	queues := make([]string, 0, len(depth))
	for q := range depth {
		queues = append(queues, q)
	}
	sort.Strings(queues)
	b.WriteString("# HELP switchtender_queue_depth Pending unclaimed runs waiting per queue.\n")
	b.WriteString("# TYPE switchtender_queue_depth gauge\n")
	for _, q := range queues {
		fmt.Fprintf(b, "switchtender_queue_depth{queue=%q} %d\n", q, depth[q])
	}
}

// writeFleetHealth emits the host gauges: how many hosts have been seen, how many are flaky, and how
// many are failing on their latest outcome.
func writeFleetHealth(b *strings.Builder, store run.Store, r *http.Request) {
	health, err := store.FleetHealth(r.Context(), defaultFleetWindow)
	if err != nil {
		return
	}
	flaky, failing := 0, 0
	for _, h := range health {
		if h.Flaky {
			flaky++
		}
		if run.FailedOutcome(h.LastOutcome) {
			failing++
		}
	}
	b.WriteString("# HELP switchtender_hosts_total Hosts seen across recent runs.\n")
	b.WriteString("# TYPE switchtender_hosts_total gauge\n")
	fmt.Fprintf(b, "switchtender_hosts_total %d\n", len(health))
	b.WriteString("# HELP switchtender_hosts_flaky Hosts flipping between failing and passing.\n")
	b.WriteString("# TYPE switchtender_hosts_flaky gauge\n")
	fmt.Fprintf(b, "switchtender_hosts_flaky %d\n", flaky)
	b.WriteString("# HELP switchtender_hosts_failing Hosts whose latest outcome failed.\n")
	b.WriteString("# TYPE switchtender_hosts_failing gauge\n")
	fmt.Fprintf(b, "switchtender_hosts_failing %d\n", failing)
}

// writeWorkers emits how many executors hold a lease and how many runs each is actively running, so
// the pool's size and load are visible.
func writeWorkers(b *strings.Builder, store run.Store, r *http.Request) {
	workers, err := store.Workers(r.Context())
	if err != nil {
		return
	}
	b.WriteString("# HELP switchtender_workers_total Executors currently holding a lease.\n")
	b.WriteString("# TYPE switchtender_workers_total gauge\n")
	fmt.Fprintf(b, "switchtender_workers_total %d\n", len(workers))
	b.WriteString("# HELP switchtender_worker_active_runs Runs an executor is running right now.\n")
	b.WriteString("# TYPE switchtender_worker_active_runs gauge\n")
	for _, wk := range workers {
		fmt.Fprintf(b, "switchtender_worker_active_runs{owner=%q} %d\n", wk.Owner, wk.Active)
	}
}

// writeRunDurations emits a histogram of run execution time from start to end over the terminal runs
// that carry both timestamps, so scrape sees the fleet's latency distribution.
func writeRunDurations(b *strings.Builder, runs []*run.Run) {
	counts := make([]int, len(durationBuckets))
	total := 0
	sum := 0.0
	for _, rn := range runs {
		if rn.StartedAt == nil || rn.EndedAt == nil {
			continue
		}
		d := rn.EndedAt.Sub(*rn.StartedAt).Seconds()
		if d < 0 {
			continue
		}
		total++
		sum += d
		for i, le := range durationBuckets {
			if d <= le {
				counts[i]++
			}
		}
	}
	b.WriteString("# HELP switchtender_run_duration_seconds Run execution time from start to end.\n")
	b.WriteString("# TYPE switchtender_run_duration_seconds histogram\n")
	for i, le := range durationBuckets {
		fmt.Fprintf(b, "switchtender_run_duration_seconds_bucket{le=\"%g\"} %d\n", le, counts[i])
	}
	fmt.Fprintf(b, "switchtender_run_duration_seconds_bucket{le=\"+Inf\"} %d\n", total)
	fmt.Fprintf(b, "switchtender_run_duration_seconds_sum %g\n", sum)
	fmt.Fprintf(b, "switchtender_run_duration_seconds_count %d\n", total)
}

// writeSpanBeats emits the span beat counters and the age of the newest beat this process wrote.
//
// A beat is suppressed when the clock has not passed the last beat, because a beat's time is a
// signed claim and a time the clock did not read would be a false statement in an attestation. The
// record only shows a bounded gap afterwards, so the condition has to be loud while it is
// happening: alert on the suppressed counter rising, or on the age passing the configured cadence.
// The age gauge is omitted until a beat exists, since zero would read as a fresh beat on an install
// that runs no beats at all.
func writeSpanBeats(b *strings.Builder, now time.Time) {
	stats := spanbeat.ReadStats()
	b.WriteString("# HELP switchtender_span_beats_total Span beats appended to the audit chain.\n")
	b.WriteString("# TYPE switchtender_span_beats_total counter\n")
	fmt.Fprintf(b, "switchtender_span_beats_total %d\n", stats.Appended)
	b.WriteString("# HELP switchtender_span_beats_suppressed_total Span beats not written because " +
		"the clock had not passed the last beat.\n")
	b.WriteString("# TYPE switchtender_span_beats_suppressed_total counter\n")
	fmt.Fprintf(b, "switchtender_span_beats_suppressed_total %d\n", stats.Suppressed)
	if stats.Last.IsZero() {
		return
	}
	age := now.Sub(stats.Last).Seconds()
	if age < 0 {
		age = 0
	}
	b.WriteString("# HELP switchtender_span_beat_age_seconds Seconds since the last span beat was " +
		"appended.\n")
	b.WriteString("# TYPE switchtender_span_beat_age_seconds gauge\n")
	fmt.Fprintf(b, "switchtender_span_beat_age_seconds %g\n", age)
}

// writeQueueWait emits a histogram of how long runs waited between submission and start, over the runs
// that carry a start time, so scrape sees whether the pool keeps up with the backlog. A run held for
// approval includes that wait, which is honest: it is time the submitter waited before work began.
func writeQueueWait(b *strings.Builder, runs []*run.Run) {
	counts := make([]int, len(queueBuckets))
	total := 0
	sum := 0.0
	for _, rn := range runs {
		if rn.StartedAt == nil {
			continue
		}
		d := rn.StartedAt.Sub(rn.CreatedAt).Seconds()
		if d < 0 {
			continue
		}
		total++
		sum += d
		for i, le := range queueBuckets {
			if d <= le {
				counts[i]++
			}
		}
	}
	b.WriteString("# HELP switchtender_run_queue_seconds Time a run waited from submission to start.\n")
	b.WriteString("# TYPE switchtender_run_queue_seconds histogram\n")
	for i, le := range queueBuckets {
		fmt.Fprintf(b, "switchtender_run_queue_seconds_bucket{le=\"%g\"} %d\n", le, counts[i])
	}
	fmt.Fprintf(b, "switchtender_run_queue_seconds_bucket{le=\"+Inf\"} %d\n", total)
	fmt.Fprintf(b, "switchtender_run_queue_seconds_sum %g\n", sum)
	fmt.Fprintf(b, "switchtender_run_queue_seconds_count %d\n", total)
}
