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

// TestEstateAtAnswersWhatWasTrueThen is the question the whole state history exists to serve.
//
// A live view holds what is true now, so before this the only answer to "what was running on the
// audit date" was that nobody could say. Each gather overwrote the one before it, and the evidence
// was destroyed by the ordinary operation of the product.
func TestEstateAtAnswersWhatWasTrueThen(t *testing.T) {
	run.SetFactsInterval(0)
	run.SetFactsDepth(run.DefaultFactsDepth)
	t.Cleanup(func() { run.SetFactsInterval(run.DefaultFactsInterval) })

	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "estate.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := d.Runs()

	march := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// web01 is upgraded in April, after the audit date.
	save := func(host, kernel string, at time.Time) {
		t.Helper()
		if err := s.SaveHostFacts(ctx, "run_"+host+kernel, []run.HostFacts{{
			Host: host, Facts: map[string]string{"kernel": kernel}, GatheredAt: at,
		}}); err != nil {
			t.Fatalf("save %s: %v", host, err)
		}
	}
	save("web01", "5.15.0", march)
	save("web01", "6.8.0", march.AddDate(0, 1, 0))
	// db01 joins the estate only in April, so it did not exist on the audit date.
	save("db01", "6.1.0", march.AddDate(0, 1, 0))

	at, err := s.EstateAt(ctx, march.Add(time.Hour))
	if err != nil {
		t.Fatalf("estate at: %v", err)
	}
	if len(at) != 1 {
		t.Fatalf("estate held %d hosts on the audit date, want 1. A host gathered only afterward "+
			"must be absent rather than invented, or the estate describes machines nobody had seen",
			len(at))
	}
	if at[0].Host != "web01" || at[0].Facts["kernel"] != "5.15.0" {
		t.Errorf("estate = %s on %s, want web01 on 5.15.0, the reading in effect then",
			at[0].Host, at[0].Facts["kernel"])
	}

	// And now, where both hosts exist and web01 carries its newer kernel.
	now, err := s.EstateAt(ctx, march.AddDate(0, 2, 0))
	if err != nil {
		t.Fatalf("estate now: %v", err)
	}
	if len(now) != 2 {
		t.Fatalf("estate holds %d hosts today, want 2", len(now))
	}
	// Ordered by host, so db01 comes first.
	if now[0].Host != "db01" || now[1].Facts["kernel"] != "6.8.0" {
		t.Errorf("estate today = %+v, want db01 then web01 on 6.8.0", now)
	}
}
