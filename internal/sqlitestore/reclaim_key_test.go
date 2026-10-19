package sqlitestore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestReclaimStaleBelowTheSecond pins that the lease sweep reclaims a lease taken on the whole
// second just before its cutoff. Stored as text, that lease reads "10:00:00Z" against a cutoff of
// "10:00:00.4Z", and the raw text puts it after the cutoff, so a sweep on the raw column left the
// stale lease held. The sweep reads the wall clock, so the test starts early in a second, which
// keeps the lease and the cutoff inside one second and the raw comparison wrong.
func TestReclaimStaleBelowTheSecond(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "reclaim.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := t.Context()
	const ttl = time.Minute
	if frac := time.Duration(time.Now().Nanosecond()); frac > 600*time.Millisecond {
		time.Sleep(time.Second - frac + 20*time.Millisecond)
	}
	stale := time.Now().Add(-ttl).Truncate(time.Second)
	fresh := time.Now().Add(-ttl / 2)
	runs := store.Runs()
	for id, at := range map[string]time.Time{"run_stale": stale, "run_fresh": fresh} {
		if err := runs.Save(ctx, &run.Run{ID: id, Playbook: "p.yml", Status: run.StatusRunning,
			CreatedAt: at, ClaimedBy: "w", ClaimedAt: &at}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	if _, err := runs.ReclaimStale(ctx, ttl); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	for id, want := range map[string]run.Status{
		"run_stale": run.StatusInterrupted, "run_fresh": run.StatusRunning,
	} {
		got, err := runs.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if got.Status != want {
			t.Errorf("%s status = %s, want %s", id, got.Status, want)
		}
	}
}
