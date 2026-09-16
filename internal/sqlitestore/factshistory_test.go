package sqlitestore

import (
	"context"
	"fmt"
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

	at, err := s.EstateAt(ctx, march.Add(time.Hour), 0)
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
	now, err := s.EstateAt(ctx, march.AddDate(0, 2, 0), 0)
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

// TestEstateAtReturnsEachHostOnce covers a query shape that quietly double counted.
//
// The first version grouped by host on the maximum gather time and joined back on equality. That
// returns a row per matching row rather than per host, so two readings of one host sharing an
// instant put it in the estate twice. An estate view that reports more hosts than an estate has is
// not an answer an audit can use, and the miscount is invisible unless somebody counts.
//
// Collisions are possible whenever the interval is zero, which is the documented opt-in for keeping
// every gather, and two runs touch the same host at the same recorded instant.
func TestEstateAtReturnsEachHostOnce(t *testing.T) {
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "once.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := d.Runs()

	same := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"run_a", "run_b"} {
		if err := s.SaveHostFacts(ctx, id, []run.HostFacts{{
			Host: "web01", Facts: map[string]string{"from": id}, GatheredAt: same,
		}}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	got, err := s.EstateAt(ctx, same.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("estate at: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("estate holds %d rows for one host, want 1. The host is counted once per reading "+
			"rather than once: %+v", len(got), got)
	}
	// Whichever reading wins, the pick has to be stable rather than whatever the engine returns
	// first, or two identical requests can disagree about the estate.
	second, err := s.EstateAt(ctx, same.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("estate at, again: %v", err)
	}
	if len(second) != 1 || second[0].RunID != got[0].RunID {
		t.Errorf("two identical requests picked %q then %q, want the same reading both times",
			got[0].RunID, second[0].RunID)
	}
}

// TestEstateAtIsBoundedByTheQuery covers memory rather than correctness.
//
// An estate is one fact set per host, and the diff reads two of them. Capping only the response
// would already have paid the cost of reading the whole fleet, and that cost scales with the fleet
// and multiplies by however many callers ask at once. The bound has to be in the query.
func TestEstateAtIsBoundedByTheQuery(t *testing.T) {
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "bound.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := d.Runs()

	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	facts := make([]run.HostFacts, 0, 50)
	for i := 0; i < 50; i++ {
		facts = append(facts, run.HostFacts{
			Host: fmt.Sprintf("host%03d", i), GatheredAt: at,
			Facts: map[string]string{"kernel": "6.8.0"},
		})
	}
	if err := s.SaveHostFacts(ctx, "run_1", facts); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.EstateAt(ctx, at.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("estate at: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("EstateAt with a limit of 10 returned %d rows, so the whole fleet was read and "+
			"the cap only trimmed what had already been paid for", len(got))
	}
	// Ordered by host, so the bound is the first N rather than an arbitrary slice. Two identical
	// requests must agree about which hosts a capped answer shows.
	if got[0].Host != "host000" || got[9].Host != "host009" {
		t.Errorf("a capped estate returned %s..%s, want the first ten hosts in order",
			got[0].Host, got[9].Host)
	}

	// Zero reads everything, for a caller that genuinely needs the whole estate.
	all, err := s.EstateAt(ctx, at.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("estate at, unbounded: %v", err)
	}
	if len(all) != 50 {
		t.Errorf("an unbounded read returned %d rows, want all 50", len(all))
	}
}
