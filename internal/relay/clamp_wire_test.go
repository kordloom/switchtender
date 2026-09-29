package relay_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestReportedTimesAreClampedThroughTheHandler proves the clamp is on the path a worker actually
// reports through, not merely available beside it.
//
// The times a worker reports are digested into the run's outcome entry and travel in its receipt, so
// they were the one part of the record a compromised relay could choose. A receipt built on them
// verified cleanly, because the forgery is in the facts rather than the math: the chain faithfully
// attests an execution window that never happened.
func TestReportedTimesAreClampedThroughTheHandler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)

	created := time.Now().Add(-10 * time.Minute)
	held := &run.Run{
		ID: "run_t", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: created,
		ClaimedBy: "worker-a", ClaimedAt: &created, ClaimSecret: "secret-a",
	}
	if err := backing.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// A worker reports progress first and terminal second, which is the sequence the two writes
	// persist: the progress write is the only one that records a start time, and the terminal write
	// is the only one that records an end time. Both are forged here to a window that never
	// happened, a year before the run existed and a year from now.
	backdated := fmt.Sprintf(`{"id":"run_t","status":"running","started_at":%q}`,
		created.Add(-365*24*time.Hour).UTC().Format(time.RFC3339Nano))
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_t/save", "secret-a",
		[]byte(backdated)); code >= 400 {
		t.Fatalf("a progress report was refused: HTTP %d", code)
	}
	postdated := fmt.Sprintf(`{"id":"run_t","status":"succeeded","ended_at":%q}`,
		created.Add(365*24*time.Hour).UTC().Format(time.RFC3339Nano))
	if code := postWithLease(t, ts.URL, "/relay/v1/runs/run_t/save", "secret-a",
		[]byte(postdated)); code >= 400 {
		t.Fatalf("a terminal report was refused: HTTP %d", code)
	}

	got, err := backing.Get(ctx, "run_t")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.StartedAt == nil || got.StartedAt.Before(created) {
		t.Errorf("started at = %v, want it held at or after the run's creation %v",
			got.StartedAt, created)
	}
	if got.EndedAt == nil || got.EndedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("ended at = %v, want it held at or before now", got.EndedAt)
	}
	if !strings.Contains(got.Warning, "outside the window") {
		t.Errorf("the record does not say the times were moved: %q", got.Warning)
	}
}
