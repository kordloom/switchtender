package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
)

// TestMetricsHandler verifies the Prometheus output carries the run-status gauges, per-queue depth,
// worker gauges, and the run-duration histogram, all computed from the store.
func TestMetricsHandler(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	base := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
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

	handler := metricsHandler(store, zap.NewNop())
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
