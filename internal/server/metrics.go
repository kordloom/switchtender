package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/spanbeat"
)

// durationBuckets are the upper bounds, in seconds, of the run-duration histogram. They span a quick
// command to a long playbook so both ends of the fleet's runtime show up.
var durationBuckets = []float64{1, 5, 10, 30, 60, 300, 600, 1800, 3600}

// queueBuckets are the upper bounds, in seconds, of the queue-wait histogram. They are tighter than the
// run-duration bounds since a healthy pool drains its backlog in seconds, not the minutes a run itself
// takes, so a rising queue wait is the early signal that the pool is undersized.
var queueBuckets = []float64{0.5, 1, 2, 5, 10, 30, 60, 300, 1800}

// metricsHistogramWindow caps how many recent runs a scrape reads to feed the duration and
// queue-wait histograms, so it reads a bounded page instead of the whole run history. It is a page
// size, not a window: runs are folded into cumulative counters once each, so the page only has to
// reach back to the previous scrape rather than hold everything a histogram describes.
const metricsHistogramWindow = 10000

// metricsHandler serves run, fleet, queue, worker, and run-duration series in the Prometheus text
// exposition format. The gauges are computed from the store at scrape time and hold no state. The
// histograms cannot be: Prometheus reads their buckets as counters, and recomputing them from the
// newest page made them fall whenever a run aged out of it, which reads as a counter reset rather
// than as a smaller number. They are accumulated instead, each run folded in once when it reaches
// a terminal state, so a scrape stays cheap however large history grows and the counters still
// only climb.
// The scrape is withheld entirely from a caller who may read no runs, rather than emitted with some
// series dropped. Its equivalents on the API already do this: GET /v1/workers and GET /v1/fleet nil
// their lists for such a caller. This endpoint took no authorizer at all, so under strict grants a
// viewer in one tenant could scrape every executor name, every queue name, the estate's host count,
// how many hosts are failing, and the audit-chain gauges for work they are refused by name
// everywhere else. Partial series would still leak the shape of the estate, so the answer is empty.
func metricsHandler(store run.Store, chain *chainHealth, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: metricsHandler: Store required")
	}
	hist := newRunHistograms()
	return func(w http.ResponseWriter, r *http.Request) {
		_, anyReadable, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: metrics read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute metrics")
			return
		}
		if !anyReadable {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			return
		}
		byStatus, err := store.RunStatusCounts(r.Context())
		if err != nil {
			log.Error("server: metrics: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute metrics")
			return
		}
		// Seven columns, not whole rows. A run carries its extra vars, steps, labels, and notification
		// targets, and decoding those for the whole window on every scrape cost more than the rest of
		// this endpoint together, for values none of the histograms read.
		runs, err := store.RunTimings(r.Context(), metricsHistogramWindow)
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
		hist.fold(runs, len(runs) >= metricsHistogramWindow)
		duration, queue, behind := hist.snapshot()
		writeRunDurations(&b, duration)
		writeQueueWait(&b, queue, behind)
		writeSpanBeats(&b, time.Now())
		if chain != nil {
			writeChainHealth(&b, chain.snapshot(r.Context()), time.Now())
		}

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

// writeRunDurations emits the cumulative histogram of run execution time from start to end. The
// counts come from the accumulator rather than from the scrape's own page, so they climb for as
// long as the process lives and Prometheus can rate them.
func writeRunDurations(b *strings.Builder, h histogramCounts) {
	b.WriteString("# HELP switchtender_run_duration_seconds Run execution time from start to end.\n")
	b.WriteString("# TYPE switchtender_run_duration_seconds histogram\n")
	for i, le := range durationBuckets {
		fmt.Fprintf(b, "switchtender_run_duration_seconds_bucket{le=\"%g\"} %d\n", le, h.counts[i])
	}
	fmt.Fprintf(b, "switchtender_run_duration_seconds_bucket{le=\"+Inf\"} %d\n", h.total)
	fmt.Fprintf(b, "switchtender_run_duration_seconds_sum %g\n", h.sum)
	fmt.Fprintf(b, "switchtender_run_duration_seconds_count %d\n", h.total)
}

// writeChainHealth emits the audit chain integrity gauges. Verification is incremental and
// cached, so a scrape stays cheap however long the trail grows. The last-anchor age is omitted
// until an anchor exists, since zero would read as a fresh anchor on an install that anchors
// nothing.
func writeChainHealth(b *strings.Builder, g chainGauges, now time.Time) {
	boolGauge := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	b.WriteString("# HELP switchtender_audit_chain_verified Whether the audit chain verifies " +
		"end to end (1 sound, 0 broken).\n")
	b.WriteString("# TYPE switchtender_audit_chain_verified gauge\n")
	fmt.Fprintf(b, "switchtender_audit_chain_verified %d\n", boolGauge(g.Verified))
	b.WriteString("# HELP switchtender_audit_chain_entries Audit entries verified so far.\n")
	b.WriteString("# TYPE switchtender_audit_chain_entries gauge\n")
	fmt.Fprintf(b, "switchtender_audit_chain_entries %d\n", g.Entries)
	b.WriteString("# HELP switchtender_audit_chain_broke_at One-based position of the first " +
		"entry that failed verification, 0 while the chain holds.\n")
	b.WriteString("# TYPE switchtender_audit_chain_broke_at gauge\n")
	fmt.Fprintf(b, "switchtender_audit_chain_broke_at %d\n", g.BrokeAt)
	b.WriteString("# HELP switchtender_audit_anchors_total Anchors recorded over the chain.\n")
	b.WriteString("# TYPE switchtender_audit_anchors_total gauge\n")
	fmt.Fprintf(b, "switchtender_audit_anchors_total %d\n", g.AnchorsTotal)
	b.WriteString("# HELP switchtender_audit_anchor_problems Anchors the chain no longer " +
		"reaches with the anchored link.\n")
	b.WriteString("# TYPE switchtender_audit_anchor_problems gauge\n")
	fmt.Fprintf(b, "switchtender_audit_anchor_problems %d\n", g.AnchorProblems)
	b.WriteString("# HELP switchtender_audit_health_stale Whether these audit gauges are from " +
		"an earlier refresh because the last one failed (1 stale).\n")
	b.WriteString("# TYPE switchtender_audit_health_stale gauge\n")
	fmt.Fprintf(b, "switchtender_audit_health_stale %d\n", boolGauge(g.Stale))
	if !g.LastAnchorAt.IsZero() {
		age := now.Sub(g.LastAnchorAt).Seconds()
		if age < 0 {
			age = 0
		}
		b.WriteString("# HELP switchtender_audit_last_anchor_age_seconds Seconds since the " +
			"newest anchor was made.\n")
		b.WriteString("# TYPE switchtender_audit_last_anchor_age_seconds gauge\n")
		fmt.Fprintf(b, "switchtender_audit_last_anchor_age_seconds %g\n", age)
	}
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

// writeQueueWait emits the cumulative histogram of how long runs waited between submission and
// start. A run held for approval includes that wait, which is honest: it is time the submitter
// waited before work began. The wait is folded in when the run reaches a terminal state, the same
// instant its duration is, so neither histogram can count a run twice.
//
// The companion counter says how many scrapes could not prove they saw every run that finished
// since the previous one. It stays at zero unless more runs finish between two scrapes than a
// single page holds, and an operator who sees it climbing is looking at series that undercount.
func writeQueueWait(b *strings.Builder, h histogramCounts, behind int) {
	b.WriteString("# HELP switchtender_run_queue_seconds Time a run waited from submission to start.\n")
	b.WriteString("# TYPE switchtender_run_queue_seconds histogram\n")
	for i, le := range queueBuckets {
		fmt.Fprintf(b, "switchtender_run_queue_seconds_bucket{le=\"%g\"} %d\n", le, h.counts[i])
	}
	fmt.Fprintf(b, "switchtender_run_queue_seconds_bucket{le=\"+Inf\"} %d\n", h.total)
	fmt.Fprintf(b, "switchtender_run_queue_seconds_sum %g\n", h.sum)
	fmt.Fprintf(b, "switchtender_run_queue_seconds_count %d\n", h.total)
	b.WriteString("# HELP switchtender_run_timing_scrapes_behind_total Scrapes that could not read " +
		"every run finished since the previous one.\n")
	b.WriteString("# TYPE switchtender_run_timing_scrapes_behind_total counter\n")
	fmt.Fprintf(b, "switchtender_run_timing_scrapes_behind_total %d\n", behind)
}
