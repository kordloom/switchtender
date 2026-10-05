package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// testHostFactsKeepTheNewerGather verifies a gather saved after a newer one, the way a run that
// gathered first and finished last saves, leaves the newer reading as the host's current facts and
// as the estate in effect afterward. Both used to take whichever run saved last.
func testHostFactsKeepTheNewerGather(t *testing.T, store run.Store) {
	ctx := context.Background()
	newer := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-30 * time.Minute)
	for _, save := range []struct {
		runID  string
		kernel string
		at     time.Time
	}{
		{runID: "run_quick", kernel: "6.8.0", at: newer},
		{runID: "run_slow", kernel: "6.1.0", at: older},
	} {
		if err := store.SaveHostFacts(ctx, save.runID, []run.HostFacts{{Host: "web01",
			Facts: map[string]string{"kernel": save.kernel}, GatheredAt: save.at}}); err != nil {
			t.Fatalf("SaveHostFacts(%s) error = %v", save.runID, err)
		}
	}
	got, err := store.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("HostFactsFor() error = %v", err)
	}
	if got.Facts["kernel"] != "6.8.0" || got.RunID != "run_quick" {
		t.Errorf("current facts = %+v, want the newer gather from run_quick", got)
	}
	estate, err := store.EstateAt(ctx, newer.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("EstateAt() error = %v", err)
	}
	if len(estate) != 1 || estate[0].Facts["kernel"] != "6.8.0" {
		t.Errorf("estate after both gathers = %+v, want web01 on the newer 6.8.0 gather", estate)
	}
	if err := store.SaveHostFacts(ctx, "run_next", []run.HostFacts{{Host: "web01",
		Facts: map[string]string{"kernel": "6.9.0"}, GatheredAt: newer.Add(time.Minute)}}); err != nil {
		t.Fatalf("SaveHostFacts(run_next) error = %v", err)
	}
	if got, err = store.HostFactsFor(ctx, "web01"); err != nil || got.Facts["kernel"] != "6.9.0" {
		t.Errorf("current facts after a later gather = %+v, %v, want 6.9.0", got, err)
	}
}
