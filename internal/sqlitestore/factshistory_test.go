package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestHostStateAccumulatesRatherThanBeingOverwritten covers the recording that every other estate
// question depends on.
//
// host_facts is keyed by host alone and upserts, so it holds exactly one reading per host and a
// gather destroys the previous one. That is correct for the question it answers, "what is this host
// now", and it meant no history of the estate existed anywhere: asking what a host looked like at
// the last audit was unanswerable, and every run was quietly destroying the evidence. This asserts
// the readings are now kept as well as replaced.
func TestHostStateAccumulatesRatherThanBeingOverwritten(t *testing.T) {
	run.SetFactsInterval(0) // Keep every gather, so the test does not depend on the clock's date.
	run.SetFactsDepth(run.DefaultFactsDepth)
	t.Cleanup(func() { run.SetFactsInterval(run.DefaultFactsInterval) })

	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "facts.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := d.Runs()

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i, kernel := range []string{"5.15.0", "6.1.0", "6.8.0"} {
		facts := []run.HostFacts{{
			Host:       "web01",
			Facts:      map[string]string{"kernel": kernel},
			GatheredAt: base.AddDate(0, 0, i),
		}}
		if err := s.SaveHostFacts(ctx, "run_"+kernel, facts); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	// The current view still answers with the newest reading, unchanged by any of this.
	now, err := s.HostFactsFor(ctx, "web01")
	if err != nil {
		t.Fatalf("host facts: %v", err)
	}
	if got := now.Facts["kernel"]; got != "6.8.0" {
		t.Errorf("current kernel = %v, want the newest reading 6.8.0", got)
	}

	// The history holds all three. Before this existed it held nothing, because nothing kept them.
	var rows int
	if err := d.db.w.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM host_facts_history WHERE host = ?", "web01").Scan(&rows); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if rows != 3 {
		t.Fatalf("history holds %d readings, want 3. Estate history is not accumulating, so no "+
			"point-in-time question can ever be answered", rows)
	}

	// The oldest reading is still readable, which is the whole point: it is what an auditor asks
	// for and what the overwrite used to destroy.
	var kernel string
	if err := d.db.w.QueryRowContext(ctx, `
SELECT json_extract(facts, '$.kernel') FROM host_facts_history
WHERE host = ? ORDER BY `+"rtrim(gathered_at, 'Z')"+` ASC LIMIT 1`, "web01").Scan(&kernel); err != nil {
		t.Fatalf("read oldest: %v", err)
	}
	if kernel != "5.15.0" {
		t.Errorf("oldest retained kernel = %q, want 5.15.0", kernel)
	}
}
