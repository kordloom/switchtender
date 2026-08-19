package dossier

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestDossierPagingCursorUsesSeqNotCount guards the event-paging cursor in hostSummaries against a bug
// the in-memory store hides. EventsAfter's contract pages by the last Seq seen, and a real store's seq
// is a global autoincrement, so one run's events sit at sparse, high seq values rather than 1..N. A
// cursor that advances by the page length instead of the last Seq never passes those high values, so it
// re-reads and re-folds the early pages, which both defeats the memory bound the paging exists for and
// double-counts per-host and per-task durations in the auditor-facing evidence. The memStore assigns
// per-run 1-based contiguous seqs, where advancing by count happens to equal the last Seq, so only a
// real store reproduces it.
func TestDossierPagingCursorUsesSeqNotCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := db.Runs()

	at := time.Now()
	// A decoy run seeds events first, pushing the global event sequence up so the target run's own events
	// begin well above its own count. This is exactly the busy-node condition that triggers the bug.
	seedRunWithEvents(t, base, "run_decoy", at, 4, false)

	// The target run has no stored per-host summaries, so the dossier folds its events, and it holds more
	// events than one page so the cursor has to advance across pages.
	const targetHosts = 10 // 10 host events + 1 stats recap = 11 events
	old := eventPage
	eventPage = 4
	t.Cleanup(func() { eventPage = old })
	seedRunWithEvents(t, base, "run_target", at, targetHosts, false)

	store := &countingEvents{Store: base}
	in, err := Collect(ctx, store, audit.NewMemStore(), "", "run_target", at)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(in.Hosts) != targetHosts {
		t.Fatalf("dossier reported %d hosts, want %d", len(in.Hosts), targetHosts)
	}

	// 11 events at a page of 4 fold in exactly three reads (4, 4, 3) when the cursor advances by the last
	// Seq. Advancing by page count re-reads the early pages, so the count climbs above this.
	const wantReads = 3
	if store.pagedReads != wantReads {
		t.Errorf("folded the event stream in %d paged reads, want %d; a higher count means the cursor "+
			"re-read pages instead of advancing by the last Seq", store.pagedReads, wantReads)
	}
	if store.largestPage > eventPage {
		t.Errorf("a page held %d events, above the %d window", store.largestPage, eventPage)
	}
}
