package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/pgstore"
)

// auditChainStore opens the shared database, clears the chain, and returns the audit store.
//
// None of the tests in this file are parallel, and that is not an oversight. A PostgreSQL database
// holds exactly one audit chain, seq is unique across it, and these tests assert on the whole chain,
// so they mutate state that is global to the package the way t.Setenv is global to a process. They
// follow the existing PostgreSQL contract tests, which truncate for the same reason.
func auditChainStore(t *testing.T) (context.Context, *pgstore.DB, audit.Store) {
	t.Helper()
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clearChain(t, dsn)
	return context.Background(), db, db.Audits()
}

// clearChain empties the audit tables so a test reasons about a chain it wrote itself.
func clearChain(t *testing.T, dsn string) {
	t.Helper()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("TRUNCATE audit_entries, audit_anchors"); err != nil {
		t.Fatalf("truncate audit tables: %v", err)
	}
}

// TestAppendRefusesAMintedSpanMarker pins the reservation that keeps the beat sequence honest. Every
// reader of the chain treats a well-formed span marker as a beat the server minted, so an ordinary
// append carrying one would inject a beat that renumbers the real ones and fails every bundle built
// over the chain. The refusal has to fail closed: nothing may be written, not even the entry minus
// its marker.
func TestAppendRefusesAMintedSpanMarker(t *testing.T) {
	ctx, _, s := auditChainStore(t)

	tests := []struct {
		Name      string
		Entry     *audit.Entry
		Want      error
		WantWrite bool
	}{{ // Test 0: The exact reserved combination is refused.
		Name: "well-formed marker",
		Entry: &audit.Entry{
			ID: "ae_forged", At: time.Now().UTC(), Actor: audit.SpanActor,
			Method: audit.SpanMethod, Path: audit.SpanPath(1, 0, 60),
		},
		Want: audit.ErrReservedSpan, WantWrite: false,
	}, { // Test 1: The actor and method alone, with a path that does not round-trip, is an
		// ordinary entry and must still be accepted, or a real request to /span is silently lost.
		Name: "near miss path",
		Entry: &audit.Entry{
			ID: "ae_nearmiss", At: time.Now().UTC(), Actor: audit.SpanActor,
			Method: audit.SpanMethod, Path: "/span/notanumber",
		},
		Want: nil, WantWrite: true,
	}, { // Test 2: The span path under a different actor is not the reserved combination.
		Name: "span path other actor",
		Entry: &audit.Entry{
			ID: "ae_otheractor", At: time.Now().UTC(), Actor: "alice",
			Method: audit.SpanMethod, Path: audit.SpanPath(1, 0, 60),
		},
		Want: nil, WantWrite: true,
	}, { // Test 3: A beat number of zero does not round-trip, so it is not a marker.
		Name: "zero beat",
		Entry: &audit.Entry{
			ID: "ae_zerobeat", At: time.Now().UTC(), Actor: audit.SpanActor,
			Method: audit.SpanMethod, Path: "/span/0?count=0&cadence_s=60",
		},
		Want: nil, WantWrite: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			before := chainLen(t, ctx, s)
			err := s.Append(ctx, test.Entry)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: Append() error = %v, want %v", test.Name, err, test.Want)
			}
			after := chainLen(t, ctx, s)
			if test.WantWrite && after != before+1 {
				t.Errorf("%s: chain went from %d to %d, want one more entry", test.Name, before, after)
			}
			if !test.WantWrite && after != before {
				t.Errorf("%s: the refused append still wrote: chain went from %d to %d. The refusal "+
					"has to fail closed or a caller can renumber every real beat.",
					test.Name, before, after)
			}
		})
	}
}

// TestAppendSpanBeatRefusesAClockThatWentBackward pins the one case where the store declines to
// record something rather than record a time it cannot stand behind. A beat's time is a signed claim
// about when the population was counted, and clocks do step backward from NTP corrections and
// resumed snapshots. Writing a time the clock did not read would make the attestation say something
// false, so the beat is skipped and nothing is written.
func TestAppendSpanBeatRefusesAClockThatWentBackward(t *testing.T) {
	ctx, _, s := auditChainStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	first, err := s.AppendSpanBeat(ctx, base, 60)
	if err != nil {
		t.Fatalf("the first beat was refused: %v", err)
	}
	if first.Path != audit.SpanPath(1, 0, 60) {
		t.Errorf("first beat path = %q, want beat one over an empty chain", first.Path)
	}

	tests := []struct {
		Name string
		At   time.Time
		Want error
	}{{ // Test 0: A step backward is refused.
		Name: "backward", At: base.Add(-time.Hour), Want: audit.ErrClockBehind,
	}, { // Test 1: The same instant does not strictly advance, so it is refused too.
		Name: "same instant", At: base, Want: audit.ErrClockBehind,
	}, { // Test 2: A lead smaller than the stored granularity is the same instant once truncated.
		Name: "sub-granularity", At: base.Add(time.Nanosecond), Want: audit.ErrClockBehind,
	}, { // Test 3: A genuine advance is accepted, so the check is not simply always closed.
		Name: "advanced", At: base.Add(time.Second), Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			before := chainLen(t, ctx, s)
			got, err := s.AppendSpanBeat(ctx, test.At, 60)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: AppendSpanBeat() error = %v, want %v", test.Name, err, test.Want)
			}
			after := chainLen(t, ctx, s)
			if test.Want != nil {
				if after != before {
					t.Errorf("%s: a refused beat still wrote %d entries. A beat carrying a time the "+
						"clock never read is a false statement in an attestation.",
						test.Name, after-before)
				}
				if got != nil {
					t.Errorf("%s: a refused beat returned an entry: %+v", test.Name, got)
				}
				return
			}
			if after != before+1 {
				t.Errorf("%s: chain went from %d to %d, want one more entry", test.Name, before, after)
			}
			// The skipped beats did not consume their numbers: the next accepted beat is two.
			// Its count is zero because no ordinary entry landed between the two beats.
			if got.Path != audit.SpanPath(2, 0, 60) {
				t.Errorf("beat path = %q, want beat two, since a refused beat leaves its number for "+
					"the next beat the chain accepts", got.Path)
			}
		})
	}
}

// TestConcurrentAppendsNeverForkTheChain pins the advisory lock that makes the trail tamper-evident
// under a real cluster. Every replica appends to one chain through its own connection, so without
// the lock two writers read the same head and both link to it: the sequence duplicates, one entry's
// prev_hash points at a sibling rather than a parent, and verification fails on a chain nobody
// tampered with. Run with -race.
func TestConcurrentAppendsNeverForkTheChain(t *testing.T) {
	ctx, _, s := auditChainStore(t)

	const writers = 8
	const each = 6
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				e := &audit.Entry{
					ID: fmt.Sprintf("ae_race_%d_%d", w, i), At: time.Now().UTC(),
					Actor: fmt.Sprintf("writer-%d", w), Method: "POST", Path: "/api/runs",
				}
				if err := s.Append(ctx, e); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append() error under concurrency = %v", err)
	}

	chain, err := s.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != writers*each {
		t.Fatalf("chain holds %d entries, want %d", len(chain), writers*each)
	}
	seen := map[int64]string{}
	for i, e := range chain {
		if prev, dup := seen[e.Seq]; dup {
			t.Fatalf("seq %d is held by both %s and %s: the chain forked, so verification fails on "+
				"a trail nobody tampered with", e.Seq, prev, e.ID)
		}
		seen[e.Seq] = e.ID
		if int64(i+1) != e.Seq {
			t.Fatalf("entry %d carries seq %d: the sequence has a gap or a repeat", i, e.Seq)
		}
		if i == 0 {
			continue
		}
		if e.PrevHash != chain[i-1].Hash {
			t.Fatalf("entry %s links to %q but its predecessor %s hashes to %q: two writers read "+
				"the same head and both linked to it", e.ID, e.PrevHash, chain[i-1].ID,
				chain[i-1].Hash)
		}
		if e.Hash == "" {
			t.Fatalf("entry %s carries no hash, so nothing commits to it", e.ID)
		}
	}
}

// TestConcurrentSpanBeatsAreNumberedOnce pins the same lock over the beat path. The store's own doc
// says readers do not block writers here, so only the lock stops two replicas minting the same beat
// number. A duplicated beat renumbers the sequence and fails every bundle covering the pair.
func TestConcurrentSpanBeatsAreNumberedOnce(t *testing.T) {
	ctx, _, s := auditChainStore(t)

	const writers = 6
	base := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	beats := map[string]int{}
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Each writer offers a distinct advancing time, so a refusal means it lost the race for
			// that instant rather than that the clock check is broken.
			got, err := s.AppendSpanBeat(ctx, base.Add(time.Duration(w+1)*time.Second), 60)
			if err != nil {
				return
			}
			mu.Lock()
			beats[got.Path]++
			mu.Unlock()
		}(w)
	}
	wg.Wait()

	for path, n := range beats {
		if n > 1 {
			t.Errorf("beat %s was minted %d times: two replicas read the same head and both "+
				"numbered from it", path, n)
		}
	}
	// Whatever survived has to be a strictly increasing run of beats starting at one.
	got, err := s.SpanBeats(ctx, 100)
	if err != nil {
		t.Fatalf("SpanBeats() error = %v", err)
	}
	for i, e := range got {
		beat, _, _, ok := audit.ParseSpanPath(e.Path)
		if !ok {
			t.Fatalf("beat %d has a path that does not round-trip: %q", i, e.Path)
		}
		if beat != int64(i+1) {
			t.Fatalf("beat %d in the feed carries number %d, want %d: the numbering is not dense",
				i, beat, i+1)
		}
	}
}

// TestSpanBeatsSkipsNearMissRowsWithoutSpendingASlot pins the documented reason SpanBeats reads more
// rows than it returns. A row wearing the span actor and method with a path that does not round-trip
// is an ordinary entry, and counting it against the caller's limit would silently shorten the beat
// feed, which is what a relying party reads to see the attested windows.
func TestSpanBeatsSkipsNearMissRowsWithoutSpendingASlot(t *testing.T) {
	ctx, _, s := auditChainStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	// Three real beats with near-miss rows interleaved between them.
	for i := range 3 {
		for j := range 2 {
			e := &audit.Entry{
				ID: fmt.Sprintf("ae_nearmiss_%d_%d", i, j), At: base,
				Actor: audit.SpanActor, Method: audit.SpanMethod,
				Path: fmt.Sprintf("/span/not-a-beat-%d-%d", i, j),
			}
			if err := s.Append(ctx, e); err != nil {
				t.Fatalf("Append(near miss) error = %v", err)
			}
		}
		if _, err := s.AppendSpanBeat(ctx, base.Add(time.Duration(i+1)*time.Second), 60); err != nil {
			t.Fatalf("AppendSpanBeat(%d) error = %v", i, err)
		}
	}

	tests := []struct {
		Limit     int
		WantCount int
	}{{ // Test 0: A limit below one is clamped up rather than returning nothing.
		Limit: 0, WantCount: 1,
	}, { // Test 1: A negative limit is clamped the same way.
		Limit: -5, WantCount: 1,
	}, { // Test 2: One beat, despite two near-miss rows sitting newer than it.
		Limit: 1, WantCount: 1,
	}, { // Test 3: Two beats, skipping the four near-miss rows among them.
		Limit: 2, WantCount: 2,
	}, { // Test 4: Asking for more than exist returns every beat, not every span-marked row.
		Limit: 50, WantCount: 3,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got, err := s.SpanBeats(ctx, test.Limit)
			if err != nil {
				t.Fatalf("SpanBeats(%d) error = %v", test.Limit, err)
			}
			if len(got) != test.WantCount {
				t.Fatalf("SpanBeats(%d) returned %d beats, want %d: a near-miss row spent a result "+
					"slot, so the attested-window feed reads short", test.Limit, len(got),
					test.WantCount)
			}
			for _, e := range got {
				if !audit.IsSpanMarker(e) {
					t.Errorf("the beat feed returned a non-beat entry %s with path %q", e.ID, e.Path)
				}
			}
			// The feed is oldest first, so beat numbers ascend.
			for i := 1; i < len(got); i++ {
				if got[i].Seq <= got[i-1].Seq {
					t.Errorf("the beat feed is not oldest first: seq %d follows %d",
						got[i].Seq, got[i-1].Seq)
				}
			}
		})
	}
}

// TestNearMissSpanRowsDoNotSupplyABeatNumber pins the other half of the same rule. lastSpan walks
// span-marked rows newest first and must skip one whose path does not round-trip, because that row
// is an ordinary entry merely wearing the actor. Reading a beat number out of it would renumber
// every beat after it.
func TestNearMissSpanRowsDoNotSupplyABeatNumber(t *testing.T) {
	ctx, _, s := auditChainStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	if _, err := s.AppendSpanBeat(ctx, base, 60); err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	// A near-miss row lands newer than the real beat, so a naive newest-first read would pick it.
	forged := &audit.Entry{
		ID: "ae_forged_beat", At: base, Actor: audit.SpanActor, Method: audit.SpanMethod,
		Path: "/span/9999?count=0&cadence_s=0",
	}
	if err := s.Append(ctx, forged); err != nil {
		t.Fatalf("Append(near miss) error = %v: cadence_s=0 does not round-trip, so this is an "+
			"ordinary entry and must be accepted", err)
	}
	got, err := s.AppendSpanBeat(ctx, base.Add(time.Second), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	beat, _, _, ok := audit.ParseSpanPath(got.Path)
	if !ok {
		t.Fatalf("the minted beat has a path that does not round-trip: %q", got.Path)
	}
	if beat != 2 {
		t.Fatalf("the next beat is numbered %d, want 2: a hand-crafted near-miss row supplied the "+
			"beat number, which renumbers every real beat after it", beat)
	}
}

// TestBindInstallStampsEveryLaterAppend pins the wiring between the setter and the entries it
// governs. The install id is folded into each entry's chain link, so a store that accepted the bind
// and then failed to stamp would produce a chain whose links commit to an identity the stored rows
// do not carry, and every read would report the trail broken at its first entry. That is the exact
// failure the schema comment records.
func TestBindInstallStampsEveryLaterAppend(t *testing.T) {
	ctx, db, s := auditChainStore(t)

	// Before the bind, entries carry no install.
	pre := &audit.Entry{ID: "ae_preinstall", At: time.Now().UTC(), Actor: "a", Method: "POST",
		Path: "/api/runs"}
	if err := s.Append(ctx, pre); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if pre.InstallID != "" {
		t.Errorf("InstallID = %q before any bind, want empty", pre.InstallID)
	}

	binder, ok := db.Audits().(audit.InstallBinder)
	if !ok {
		t.Fatal("the PostgreSQL audit store does not satisfy audit.InstallBinder, so the server " +
			"can never bind an install and every entry hashes without one")
	}
	binder.BindInstall("inst_test")

	post := &audit.Entry{ID: "ae_postinstall", At: time.Now().UTC(), Actor: "a", Method: "POST",
		Path: "/api/runs"}
	if err := s.Append(ctx, post); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if post.InstallID != "inst_test" {
		t.Fatalf("InstallID = %q after BindInstall, want inst_test: the link commits to the install, "+
			"so an unstamped row reads as a broken chain", post.InstallID)
	}
	// The stamp has to survive the round trip, or the read path recomputes a different link.
	chain, err := s.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("chain holds %d entries, want 2", len(chain))
	}
	if chain[1].InstallID != "inst_test" {
		t.Errorf("the stored install id read back as %q, want inst_test", chain[1].InstallID)
	}
	if chain[0].InstallID != "" {
		t.Errorf("an entry written before the bind read back with install %q", chain[0].InstallID)
	}
	// Append mutates the caller's entry in place only on success, so the returned link must match.
	if chain[1].Hash != post.Hash || chain[1].PrevHash != post.PrevHash {
		t.Error("the entry Append handed back does not match the row it stored, so a caller " +
			"recording a receipt records a link the chain does not hold")
	}
}

// TestChainScanStreamsTheSameOrderChainReturns pins the streaming reader against the materializing
// one. Verification of a long trail runs through ChainScan, so a scan that skipped an entry or
// reordered two would report a chain sound that Chain reports broken, or the reverse.
func TestChainScanStreamsTheSameOrderChainReturns(t *testing.T) {
	ctx, _, s := auditChainStore(t)
	for i := range 12 {
		e := &audit.Entry{ID: fmt.Sprintf("ae_scan_%02d", i), At: time.Now().UTC(),
			Actor: "a", Method: "POST", Path: "/api/runs"}
		if err := s.Append(ctx, e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	whole, err := s.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}

	tests := []struct {
		AfterSeq  int64
		WantCount int
	}{{ // Test 0: From the start, the scan yields the whole chain.
		AfterSeq: 0, WantCount: 12,
	}, { // Test 1: A negative cursor is still before the first entry.
		AfterSeq: -1, WantCount: 12,
	}, { // Test 2: Resuming mid-chain yields exactly the tail.
		AfterSeq: 5, WantCount: 7,
	}, { // Test 3: A cursor at the head yields nothing rather than repeating the last entry.
		AfterSeq: 12, WantCount: 0,
	}, { // Test 4: A cursor past the head is empty, not an error.
		AfterSeq: 9999, WantCount: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			var streamed []*audit.Entry
			err := s.ChainScan(ctx, test.AfterSeq, func(e *audit.Entry) error {
				cp := *e
				streamed = append(streamed, &cp)
				return nil
			})
			if err != nil {
				t.Fatalf("ChainScan(%d) error = %v", test.AfterSeq, err)
			}
			if len(streamed) != test.WantCount {
				t.Fatalf("ChainScan(%d) yielded %d entries, want %d", test.AfterSeq, len(streamed),
					test.WantCount)
			}
			offset := len(whole) - test.WantCount
			for i, e := range streamed {
				want := whole[offset+i]
				if e.ID != want.ID || e.Seq != want.Seq || e.Hash != want.Hash {
					t.Fatalf("streamed entry %d is %s/%d, want %s/%d: the streaming verifier and "+
						"the materializing one disagree about the chain", i, e.ID, e.Seq, want.ID,
						want.Seq)
				}
			}
		})
	}
}

// TestChainScanStopsAndReportsTheCallbacksError pins that a verifier can abort a long scan. The
// callback's error is returned unwrapped so the caller can match its own sentinel, and iteration
// must stop rather than run the remaining rows through a callback that already gave up.
func TestChainScanStopsAndReportsTheCallbacksError(t *testing.T) {
	ctx, _, s := auditChainStore(t)
	for i := range 10 {
		e := &audit.Entry{ID: fmt.Sprintf("ae_abort_%02d", i), At: time.Now().UTC(),
			Actor: "a", Method: "POST", Path: "/api/runs"}
		if err := s.Append(ctx, e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	sentinel := errors.New("verifier gave up")
	seen := 0
	err := s.ChainScan(ctx, 0, func(*audit.Entry) error {
		seen++
		if seen == 3 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ChainScan() error = %v, want the callback's own error so a verifier can match "+
			"its sentinel", err)
	}
	if seen != 3 {
		t.Errorf("the callback ran %d times, want 3: the scan kept feeding a verifier that had "+
			"already stopped", seen)
	}
}

// TestAnchorLifecycle pins the anchor store, which fixes chain links somewhere this install cannot
// rewrite alone. The missing-anchor refusal matters most: a delete that silently succeeded would
// let an operator believe an anchor was removed, or a retry loop believe it removed one that was
// never there.
func TestAnchorLifecycle(t *testing.T) {
	ctx, _, base0 := auditChainStore(t)
	s := anchorStore(t, base0)
	base := time.Now().UTC().Truncate(time.Second)

	for i := range 4 {
		a := &audit.Anchor{
			ID: fmt.Sprintf("an_%d", i), Type: "rfc3161", Shape: "linear",
			Seq: int64((i + 1) * 10), Link: fmt.Sprintf("hash%d", i), At: base,
			Ref: "https://tsa.example/x", InstallID: "inst_test",
		}
		if err := s.SaveAnchor(ctx, a); err != nil {
			t.Fatalf("SaveAnchor(%s) error = %v", a.ID, err)
		}
	}

	tests := []struct {
		Name      string
		Seq       int64
		WantCount int
	}{{ // Test 0: Zero means no range in mind, so every anchor comes back.
		Name: "zero", Seq: 0, WantCount: 4,
	}, { // Test 1: A negative seq is treated the same way rather than returning nothing.
		Name: "negative", Seq: -1, WantCount: 4,
	}, { // Test 2: The bound is inclusive, so a seq landing exactly on an anchor includes it.
		Name: "exact boundary", Seq: 20, WantCount: 2,
	}, { // Test 3: One below that boundary excludes it, pinning the off-by-one.
		Name: "one below", Seq: 19, WantCount: 1,
	}, { // Test 4: Below every anchor returns none.
		Name: "below all", Seq: 1, WantCount: 0,
	}, { // Test 5: Above every anchor returns all of them.
		Name: "above all", Seq: 9999, WantCount: 4,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got, err := s.Anchors(ctx, test.Seq)
			if err != nil {
				t.Fatalf("Anchors(%d) error = %v", test.Seq, err)
			}
			if len(got) != test.WantCount {
				t.Fatalf("%s: Anchors(%d) returned %d, want %d", test.Name, test.Seq, len(got),
					test.WantCount)
			}
			for i := 1; i < len(got); i++ {
				if got[i].Seq < got[i-1].Seq {
					t.Errorf("anchors are not oldest first: seq %d follows %d", got[i].Seq,
						got[i-1].Seq)
				}
			}
		})
	}

	t.Run("test 6", func(t *testing.T) { // Test 6: Deleting an anchor that is not there is refused.
		err := s.DeleteAnchor(ctx, "an_no_such_anchor")
		if !errors.Is(err, audit.ErrAnchorNotFound) {
			t.Errorf("DeleteAnchor() on a missing anchor = %v, want audit.ErrAnchorNotFound: a "+
				"silent success tells an operator an anchor was removed when none was", err)
		}
	})
	t.Run("test 7", func(t *testing.T) { // Test 7: Deleting a real anchor removes exactly one.
		if err := s.DeleteAnchor(ctx, "an_1"); err != nil {
			t.Fatalf("DeleteAnchor() error = %v", err)
		}
		got, err := s.Anchors(ctx, 0)
		if err != nil {
			t.Fatalf("Anchors() error = %v", err)
		}
		if len(got) != 3 {
			t.Errorf("after deleting one anchor %d remain, want 3", len(got))
		}
		for _, a := range got {
			if a.ID == "an_1" {
				t.Error("the deleted anchor is still listed")
			}
		}
	})
	t.Run("test 8", func(t *testing.T) { // Test 8: The same delete twice is refused the second time.
		if err := s.DeleteAnchor(ctx, "an_1"); !errors.Is(err, audit.ErrAnchorNotFound) {
			t.Errorf("the second DeleteAnchor() = %v, want audit.ErrAnchorNotFound", err)
		}
	})
}

// TestAnchorRoundTripsEveryField pins that nothing an anchor carries is dropped by the store. The
// shape and install id decide which coordinate space a link lives in and whose identity it was
// computed under, so an anchor that lost either would have a relying party check a Merkle root as
// though it were an entry hash, or read a replica's chain as this install's history.
func TestAnchorRoundTripsEveryField(t *testing.T) {
	ctx, _, base0 := auditChainStore(t)
	s := anchorStore(t, base0)
	want := &audit.Anchor{
		ID: "an_roundtrip", Type: "rfc3161", Shape: "tree", Seq: 4242,
		Link: "abc123", At: time.Now().UTC().Truncate(time.Second),
		Ref: "https://tsa.example/token", Proof: "YmFzZTY0", InstallID: "inst_round",
	}
	if err := s.SaveAnchor(ctx, want); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	got, err := s.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Anchors() returned %d anchors, want 1", len(got))
	}
	a := got[0]
	if a.ID != want.ID || a.Type != want.Type || a.Shape != want.Shape || a.Seq != want.Seq ||
		a.Link != want.Link || a.Ref != want.Ref || a.Proof != want.Proof ||
		a.InstallID != want.InstallID {
		t.Errorf("anchor round trip mismatch:\n got %+v\nwant %+v", a, want)
	}
	if !a.At.Equal(want.At) {
		t.Errorf("At = %v, want %v", a.At, want.At)
	}
}

// TestAuditEntryRoundTripsUnicodeAndLongValues pins that the trail records what actually happened
// rather than a value the database mangled. An audit path carries whatever a caller requested, so
// unicode, a very long path, and an empty optional field all have to survive: a trail that silently
// rewrites what it records is not evidence.
func TestAuditEntryRoundTripsUnicodeAndLongValues(t *testing.T) {
	ctx, _, s := auditChainStore(t)

	tests := []struct {
		Name  string
		Entry *audit.Entry
	}{{ // Test 0: Unicode in every text field.
		Name: "unicode",
		Entry: &audit.Entry{ID: "ae_uni", Actor: "élève-管理者", ActorType: "human",
			OnBehalfOf: "øystein", Method: "POST", Path: "/api/runs/日本語"},
	}, { // Test 1: A very long path, well past any column a fixed width would have allowed.
		Name: "long path",
		Entry: &audit.Entry{ID: "ae_long", Actor: "a", Method: "POST",
			Path: "/api/" + strings.Repeat("segment/", 4000)},
	}, { // Test 2: Every optional field empty, the shape of an entry with no body.
		Name:  "minimal",
		Entry: &audit.Entry{ID: "ae_min", Actor: "", Method: "GET", Path: "/"},
	}, { // Test 3: A four-byte emoji, which is where a narrow encoding would break.
		Name: "astral plane",
		Entry: &audit.Entry{ID: "ae_emoji", Actor: "🔐", Method: "DELETE",
			Path: "/api/credentials/🗝️"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			test.Entry.At = time.Now().UTC().Truncate(time.Microsecond)
			if err := s.Append(ctx, test.Entry); err != nil {
				t.Fatalf("%s: Append() error = %v", test.Name, err)
			}
			chain, err := s.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			var got *audit.Entry
			for _, e := range chain {
				if e.ID == test.Entry.ID {
					got = e
				}
			}
			if got == nil {
				t.Fatalf("%s: the entry is not in the chain", test.Name)
			}
			if got.Actor != test.Entry.Actor {
				t.Errorf("%s: Actor round trip mismatch:\n got %q\nwant %q", test.Name, got.Actor,
					test.Entry.Actor)
			}
			if got.Path != test.Entry.Path {
				t.Errorf("%s: Path round trip changed length %d to %d", test.Name,
					len(test.Entry.Path), len(got.Path))
			}
			if got.OnBehalfOf != test.Entry.OnBehalfOf {
				t.Errorf("%s: OnBehalfOf = %q, want %q", test.Name, got.OnBehalfOf,
					test.Entry.OnBehalfOf)
			}
			if !got.At.Equal(test.Entry.At) {
				t.Errorf("%s: At = %v, want %v", test.Name, got.At, test.Entry.At)
			}
		})
	}
}

// TestListClampsItsLimit pins the read side's boundary. A limit of zero reaching the database
// unchanged would return nothing, and the audit view would report an empty trail on an install that
// has one, which reads as evidence of erasure.
func TestListClampsItsLimit(t *testing.T) {
	ctx, _, s := auditChainStore(t)
	for i := range 5 {
		e := &audit.Entry{ID: fmt.Sprintf("ae_list_%d", i), At: time.Now().UTC(),
			Actor: "a", Method: "POST", Path: "/api/runs"}
		if err := s.Append(ctx, e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	tests := []struct {
		Limit     int
		WantCount int
	}{{ // Test 0: Zero is clamped to one rather than returning nothing.
		Limit: 0, WantCount: 1,
	}, { // Test 1: A negative limit is clamped the same way.
		Limit: -100, WantCount: 1,
	}, { // Test 2: One returns exactly one.
		Limit: 1, WantCount: 1,
	}, { // Test 3: A limit past the chain length returns the whole chain.
		Limit: 500, WantCount: 5,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got, err := s.List(ctx, test.Limit)
			if err != nil {
				t.Fatalf("List(%d) error = %v", test.Limit, err)
			}
			if len(got) != test.WantCount {
				t.Fatalf("List(%d) returned %d entries, want %d", test.Limit, len(got),
					test.WantCount)
			}
			// List is newest first, the opposite of Chain.
			for i := 1; i < len(got); i++ {
				if got[i].Seq >= got[i-1].Seq {
					t.Errorf("List is not newest first: seq %d follows %d", got[i].Seq, got[i-1].Seq)
				}
			}
		})
	}
}

// chainLen reports how many entries the chain currently holds.
func chainLen(t *testing.T, ctx context.Context, s audit.Store) int {
	t.Helper()
	chain, err := s.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	return len(chain)
}

// anchorStore narrows an audit.Store to the anchor half of the contract. The two are separate
// interfaces, so a store satisfying one and not the other compiles everywhere and fails only when a
// relying party asks for the anchors that fix the chain.
func anchorStore(t *testing.T, s audit.Store) audit.AnchorStore {
	t.Helper()
	as, ok := s.(audit.AnchorStore)
	if !ok {
		t.Fatal("the PostgreSQL audit store does not satisfy audit.AnchorStore, so nothing can " +
			"fix a chain link anywhere this install cannot rewrite alone")
	}
	return as
}
