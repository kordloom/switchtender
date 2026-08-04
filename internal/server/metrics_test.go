package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestMetricsHandler verifies the Prometheus output carries the run-status gauges, per-queue depth,
// worker gauges, and the run-duration histogram, all computed from the store.
func TestMetricsHandler(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	// The lease stamp must fall inside run.WorkerWindow for the worker gauges to count it.
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	started := base
	ended := base.Add(45 * time.Second)

	// Two pending runs, one on the default queue and one on dmz; a running run claimed by a worker;
	// and a succeeded run with a 45s duration to land in the histogram.
	seed := []*run.Run{
		{ID: "run_p1", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_p2", Status: run.StatusPending, CreatedAt: base, Queue: "dmz"},
		{
			ID: "run_r1", Status: run.StatusRunning, CreatedAt: base,
			ClaimedBy: "worker-a", ClaimedAt: &base, StartedAt: &started,
		},
		{ID: "run_ok", Status: run.StatusSucceeded, CreatedAt: base, StartedAt: &started, EndedAt: &ended},
	}
	for _, rn := range seed {
		if err := store.Save(ctx, rn); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	handler := metricsHandler(store, nil, zap.NewNop())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	wants := []string{
		`switchtender_runs_total{status="pending"} 2`,
		`switchtender_runs_total{status="running"} 1`,
		`switchtender_runs_total{status="succeeded"} 1`,
		`# TYPE switchtender_queue_depth gauge`,
		`switchtender_queue_depth{queue=""} 1`,
		`switchtender_queue_depth{queue="dmz"} 1`,
		`# TYPE switchtender_workers_total gauge`,
		`switchtender_workers_total 1`,
		`switchtender_worker_active_runs{owner="worker-a"} 1`,
		`# TYPE switchtender_run_duration_seconds histogram`,
		`switchtender_run_duration_seconds_bucket{le="10"} 0`,
		`switchtender_run_duration_seconds_bucket{le="60"} 1`,
		`switchtender_run_duration_seconds_bucket{le="+Inf"} 1`,
		`switchtender_run_duration_seconds_count 1`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the prometheus text format", ct)
	}
}

// seedChain appends n audit entries and returns the store.
func seedChain(t *testing.T, n int) audit.Store {
	t.Helper()
	audits := audit.NewMemStore()
	for i := 0; i < n; i++ {
		e := &audit.Entry{ID: audit.NewID(), At: time.Now().UTC(), Actor: "tester",
			Method: "POST", Path: "/v1/things"}
		if err := audits.Append(context.Background(), e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return audits
}

// scrape runs the handler once and returns the body.
func scrape(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestMetricsChainGauges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := seedChain(t, 3)
	health := newChainHealth(audits)
	handler := metricsHandler(store, health, zap.NewNop())

	body := scrape(t, handler)
	for _, want := range []string{
		"switchtender_audit_chain_verified 1",
		"switchtender_audit_chain_entries 3",
		"switchtender_audit_chain_broke_at 0",
		"switchtender_audit_anchors_total 0",
		"switchtender_audit_anchor_problems 0",
		"switchtender_audit_health_stale 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "switchtender_audit_last_anchor_age_seconds") {
		t.Error("anchor age served with no anchors; zero would read as a fresh anchor")
	}

	// The chain grows between scrapes; the incremental walk picks the new entries up.
	e := &audit.Entry{ID: audit.NewID(), At: time.Now().UTC(), Actor: "tester",
		Method: "POST", Path: "/v1/things"}
	if err := audits.Append(ctx, e); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if body = scrape(t, handler); !strings.Contains(body, "switchtender_audit_chain_entries 4") {
		t.Error("metrics did not advance to the appended entry")
	}

	// A sound anchor is counted and aged; a false one is a problem the gauge names.
	anchored := audits.(audit.AnchorStore)
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if err := anchored.SaveAnchor(ctx, &audit.Anchor{ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS,
		Seq: chain[1].Seq, Link: chain[1].Hash, At: time.Now().UTC(), Ref: "https://x"}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	body = scrape(t, handler)
	for _, want := range []string{
		"switchtender_audit_anchors_total 1",
		"switchtender_audit_anchor_problems 0",
		"switchtender_audit_last_anchor_age_seconds",
		"switchtender_audit_chain_verified 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q after a sound anchor", want)
		}
	}
	if err := anchored.SaveAnchor(ctx, &audit.Anchor{ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS,
		Seq: chain[2].Seq, Link: "not-the-link", At: time.Now().UTC(), Ref: "https://x"}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	body = scrape(t, handler)
	if !strings.Contains(body, "switchtender_audit_anchors_total 2") ||
		!strings.Contains(body, "switchtender_audit_anchor_problems 1") {
		t.Errorf("metrics did not name the false anchor:\n%s", grepLines(body, "switchtender_audit"))
	}
}

func TestMetricsChainGaugesReportABreak(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	audits := seedChain(t, 3)
	health := newChainHealth(&tamperedChain{Store: audits, at: 2})
	handler := metricsHandler(store, health, zap.NewNop())
	body := scrape(t, handler)
	if !strings.Contains(body, "switchtender_audit_chain_verified 0") ||
		!strings.Contains(body, "switchtender_audit_chain_broke_at 2") {
		t.Errorf("metrics did not report the break:\n%s", grepLines(body, "switchtender_audit"))
	}
}

func TestMetricsChainGaugesGoStaleOnReadFailure(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	audits := seedChain(t, 2)
	failing := &failingChain{Store: audits}
	health := newChainHealth(failing)
	handler := metricsHandler(store, health, zap.NewNop())

	if body := scrape(t, handler); !strings.Contains(body, "switchtender_audit_health_stale 0") {
		t.Fatalf("healthy scrape reads stale:\n%s", grepLines(body, "switchtender_audit"))
	}
	failing.fail.Store(true)
	body := scrape(t, handler)
	// A read failure is not a broken chain: the last sound values stand, marked stale.
	if !strings.Contains(body, "switchtender_audit_health_stale 1") ||
		!strings.Contains(body, "switchtender_audit_chain_verified 1") ||
		!strings.Contains(body, "switchtender_audit_chain_entries 2") {
		t.Errorf("read failure not reported as staleness:\n%s", grepLines(body, "switchtender_audit"))
	}
}

// tamperedChain rewrites one entry's hash as it streams, the in-process stand-in for a database
// edit underneath a running server.
type tamperedChain struct {
	audit.Store
	// at is the seq whose hash is corrupted.
	at int64
}

// ChainScan streams the underlying chain with the tampered entry substituted.
func (c *tamperedChain) ChainScan(ctx context.Context, afterSeq int64, fn func(*audit.Entry) error) error {
	return c.Store.ChainScan(ctx, afterSeq, func(e *audit.Entry) error {
		if e.Seq == c.at {
			e.Hash = "0000"
		}
		return fn(e)
	})
}

// failingChain fails every scan once told to.
type failingChain struct {
	audit.Store
	// fail flips the scan into failure.
	fail atomic.Bool
}

// ChainScan errors when failing, otherwise defers to the store.
func (c *failingChain) ChainScan(ctx context.Context, afterSeq int64, fn func(*audit.Entry) error) error {
	if c.fail.Load() {
		return fmt.Errorf("the disk went away")
	}
	return c.Store.ChainScan(ctx, afterSeq, fn)
}

// grepLines returns the lines of body containing needle, for failure messages.
func grepLines(body, needle string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
