// Package audittest provides a shared behavior contract for audit.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package audittest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/idgen"
)

// Contract runs the audit.Store contract against a fresh store from newStore.
func Contract(t *testing.T, rawStore func() audit.Store) {
	t.Helper()
	// The backends hand out one store struct for the whole contract and truncate the table between
	// subtests, so install binding, which lives on the struct rather than the table, would leak from
	// one subtest into the next. A subtest that binds an install would then leave every later store
	// bound, and one that expected the unbound default would silently append bound entries. Reset the
	// binding on every handout so each subtest starts from the state a fresh install has.
	newStore := func() audit.Store {
		s := rawStore()
		if b, ok := s.(audit.InstallBinder); ok {
			b.BindInstall("")
		}
		return s
	}
	t.Run("append and list", func(t *testing.T) { testAppendList(t, newStore()) })
	t.Run("an entry is read back by its id", func(t *testing.T) { testEntryByID(t, newStore()) })
	t.Run("chain verifies", func(t *testing.T) { testChain(t, newStore()) })
	t.Run("anchors round trip and scope", func(t *testing.T) { testAnchors(t, newStore()) })
	t.Run("anchor delete removes exactly the named anchor", func(t *testing.T) {
		testAnchorDelete(t, newStore())
	})
	t.Run("witness attestations round trip and scope", func(t *testing.T) {
		testAttestations(t, newStore())
	})
	t.Run("witness attestation delete removes exactly the named one", func(t *testing.T) {
		testAttestationDelete(t, newStore())
	})
	t.Run("a held witness attestation is never overwritten", func(t *testing.T) {
		testAttestationHeld(t, newStore())
	})
	t.Run("concurrent saves of one witness coordinate keep one record", func(t *testing.T) {
		testAttestationConcurrent(t, newStore())
	})
	t.Run("concurrent appends do not fork", func(t *testing.T) { testConcurrentAppend(t, newStore()) })
	t.Run("the chain says which install it belongs to", func(t *testing.T) {
		testBoundInstall(t, newStore())
	})
	t.Run("a chosen time behind the head is pinned forward", func(t *testing.T) {
		testAppendPinsBehindClock(t, newStore())
	})
	t.Run("span beats increment with counts", func(t *testing.T) { testSpanBeats(t, newStore()) })
	t.Run("span beat one adopts prior history", func(t *testing.T) { testSpanAdoption(t, newStore()) })
	t.Run("concurrent span beats never collide", func(t *testing.T) {
		testConcurrentSpanBeats(t, newStore())
	})
	t.Run("span beat refuses a clock behind the last beat", func(t *testing.T) {
		testSpanBeatAdvances(t, newStore())
	})
	t.Run("span beat refuses a time behind the newest entry", func(t *testing.T) {
		testSpanBeatBehindHead(t, newStore())
	})
	t.Run("span beat with no time reads the store clock", func(t *testing.T) {
		testSpanBeatReadsTheStoreClock(t, newStore())
	})
	t.Run("a run's outcome is appended once", func(t *testing.T) { testOutcomeOnce(t, newStore()) })
	t.Run("a fast clock does not drag the chain forward", func(t *testing.T) {
		testFastClockIsHeld(t, newStore())
	})
	t.Run("ordinary append refuses the span marker", func(t *testing.T) {
		testReservedSpan(t, newStore())
	})
	t.Run("near-miss span entry stays ordinary", func(t *testing.T) {
		testNearMissSpan(t, newStore())
	})
	t.Run("span beats query filters and limits store-side", func(t *testing.T) {
		testSpanBeatsQuery(t, newStore())
	})
	t.Run("content and actor fields round trip and are committed", func(t *testing.T) {
		testCommittedFields(t, newStore())
	})
	t.Run("a bound install round trips and the chain still verifies", func(t *testing.T) {
		testInstallBinding(t, newStore())
	})
	t.Run("empty list is non-nil", func(t *testing.T) {
		got, err := newStore().List(context.Background(), 10)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if got == nil {
			t.Error("List() on an empty store = nil, want a non-nil empty slice")
		}
	})
}

// appendMutations appends n ordinary entries so a span test has history to count.
func appendMutations(t *testing.T, store audit.Store, base time.Time, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "root", Method: "POST", Path: "/runs",
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
}

// checkBeat asserts one appended span beat carries the expected beat, count, and cadence, and that
// its recorded time was truncated to microseconds, since a nanosecond beat could never be verified
// by a third party and would poison every bundle after it.
func checkBeat(t *testing.T, e *audit.Entry, at time.Time, wantBeat, wantCount int64, wantCadence int) {
	t.Helper()
	if e == nil {
		t.Fatal("AppendSpanBeat() returned a nil entry")
	}
	if e.Actor != audit.SpanActor || e.Method != audit.SpanMethod {
		t.Errorf("beat entry actor %q method %q, want %q %q", e.Actor, e.Method,
			audit.SpanActor, audit.SpanMethod)
	}
	beat, count, cadence, ok := audit.ParseSpanPath(e.Path)
	if !ok {
		t.Fatalf("beat path %q does not parse back", e.Path)
	}
	if beat != wantBeat || count != wantCount || cadence != wantCadence {
		t.Errorf("beat = %d count = %d cadence = %d, want %d %d %d",
			beat, count, cadence, wantBeat, wantCount, wantCadence)
	}
	if !e.At.Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("beat at = %s, want %s truncated to microseconds", e.At, at)
	}
}

// testSpanBeats verifies beats increment by exactly one with the right counts as ordinary appends
// interleave: the count is how many entries landed since the previous beat, and a beat right after
// another counts zero.
func testSpanBeats(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 1500, time.UTC)

	first, err := store.AppendSpanBeat(ctx, base, 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	checkBeat(t, first, base, 1, 0, 60)

	appendMutations(t, store, base.Add(time.Minute), 2)
	second, err := store.AppendSpanBeat(ctx, base.Add(time.Hour), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() second error = %v", err)
	}
	checkBeat(t, second, base.Add(time.Hour), 2, 2, 60)

	third, err := store.AppendSpanBeat(ctx, base.Add(2*time.Hour), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() third error = %v", err)
	}
	checkBeat(t, third, base.Add(2*time.Hour), 3, 0, 60)

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 5 {
		t.Fatalf("Chain() len = %d, want 5", len(chain))
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d after span beats", at)
	}
	if head := chain[len(chain)-1]; head.Seq != third.Seq || head.Hash != third.Hash {
		t.Errorf("chain head = seq %d hash %q, want the returned beat seq %d hash %q",
			head.Seq, head.Hash, third.Seq, third.Hash)
	}
}

// testSpanAdoption verifies the first beat on an already-populated chain counts every prior entry,
// which is what lets a mid-life install adopt beats without renumbering its history.
func testSpanAdoption(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	appendMutations(t, store, base, 5)

	beat, err := store.AppendSpanBeat(ctx, base.Add(time.Hour), 300)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	checkBeat(t, beat, base.Add(time.Hour), 1, 5, 300)
	if beat.Seq != 6 {
		t.Errorf("beat seq = %d, want 6", beat.Seq)
	}
}

// testSpanBeatAdvances verifies a beat is written only when its time strictly leads the beat before
// it, and that a caller's time that does not is refused with nothing written.
//
// A verifier fails a bundle outright when a beat's time does not advance past its predecessor,
// where a cadence gap is only reported, and a link commits to the time its entry holds, so neither
// beat of such a pair can ever be repaired. Clocks step backward from an NTP correction on a
// running server, a virtual machine resuming from a snapshot, or a restart after an offline fix.
// The store refuses those beats rather than moving them forward to fit, because a beat's time is a
// signed claim: a time the clock never read would be a false statement in an attestation. The
// number is not consumed, so the beat that eventually lands carries it and the numbering stays
// contiguous, which is what keeps a skipped beat a reported gap instead of a deleted window.
func testSpanBeatAdvances(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	first, err := store.AppendSpanBeat(ctx, base, 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}

	// The clock stepped an hour backward, and an equal reading is what a verifier rejects too, not
	// only an earlier one. Both are refused, and neither may leave anything behind in the chain.
	for _, at := range []time.Time{base.Add(-time.Hour), base} {
		refused, err := store.AppendSpanBeat(ctx, at, 60)
		if !errors.Is(err, audit.ErrClockBehind) {
			t.Fatalf("AppendSpanBeat() at %s error = %v, want audit.ErrClockBehind: a beat whose "+
				"time is a signed claim must not be written at a time the clock did not read", at, err)
		}
		if refused != nil {
			t.Errorf("AppendSpanBeat() at %s returned entry %+v, want nil", at, refused)
		}
		checkHead(t, store, first, 1)
	}

	// Once real time passes the last beat the cadence resumes on its own, at the caller's own time.
	ahead, err := store.AppendSpanBeat(ctx, base.Add(time.Hour), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() with a recovered clock error = %v", err)
	}
	if want := base.Add(time.Hour); !ahead.At.Equal(want) {
		t.Errorf("beat two at = %s, want the caller's time %s exactly", ahead.At, want)
	}
	// The refused beats did not burn their number: a missing beat number is a deleted window and
	// fails a bundle, where the longer interval is only reported as a gap.
	checkBeat(t, ahead, base.Add(time.Hour), 2, 0, 60)

	// The advance has to hold in what was stored, not just in what was handed back.
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("Chain() len = %d, want 2: only the two accepted beats", len(chain))
	}
	var prev *audit.Entry
	for i, e := range chain {
		if !audit.IsSpanMarker(e) {
			t.Fatalf("chain entry %d is not a span beat: %+v", i, e)
		}
		if prev != nil && !e.At.After(prev.At) {
			t.Errorf("stored beat %d at %s does not advance past %s", i, e.At, prev.At)
		}
		prev = e
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d after a backward clock", at)
	}
}

// checkHead asserts the chain still ends at want and holds wantLen entries, so a refused beat is
// shown to have written nothing rather than merely returned an error.
func checkHead(t *testing.T, store audit.Store, want *audit.Entry, wantLen int) {
	t.Helper()
	chain, err := store.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != wantLen {
		t.Fatalf("Chain() len = %d after a refused beat, want %d: a refused beat writes nothing",
			len(chain), wantLen)
	}
	head := chain[len(chain)-1]
	if head.Seq != want.Seq || head.Hash != want.Hash || !head.At.Equal(want.At) {
		t.Errorf("chain head = seq %d hash %q at %s, want seq %d hash %q at %s",
			head.Seq, head.Hash, head.At, want.Seq, want.Hash, want.At)
	}
}

// testConcurrentSpanBeats fires many beats at once and checks they mint distinct consecutive beat
// numbers. This is the HA race: two replicas reading the same last beat and both appending its
// successor would produce a duplicate, and a duplicate or skipped beat fails every bundle built
// over the chain, so beat assignment must serialize in the store rather than in one process.
func testConcurrentSpanBeats(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	deadline := time.Now().Add(30 * time.Second)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Whichever caller loses the race by a microsecond reads a clock that has not passed the
			// beat the winner just wrote, and the store refuses it rather than recording a time that
			// was never read. Here that means retrying with a fresh reading, which is what the
			// emitter does on its next tick, so every beat still lands and the numbering is checked
			// end to end.
			for {
				_, err := store.AppendSpanBeat(ctx, time.Now().UTC(), 60)
				switch {
				case err == nil:
					return
				case !errors.Is(err, audit.ErrClockBehind) || time.Now().After(deadline):
					errs <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AppendSpanBeat() error = %v", err)
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != n {
		t.Fatalf("Chain() len = %d, want %d", len(chain), n)
	}
	for i, e := range chain {
		beat, count, _, ok := audit.ParseSpanPath(e.Path)
		if !ok || e.Actor != audit.SpanActor || e.Method != audit.SpanMethod {
			t.Fatalf("entry %d is not a span beat: %+v", i, e)
		}
		// Beats must come out 1..n in chain order: a repeat is a duplicate and a jump is a skip,
		// and either fails a bundle.
		if beat != int64(i+1) {
			t.Errorf("entry %d beat = %d, want %d", i, beat, i+1)
		}
		if count != 0 {
			t.Errorf("entry %d count = %d, want 0 between back-to-back beats", i, count)
		}
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d after concurrent beats", at)
	}
}

// testReservedSpan verifies ordinary Append refuses an entry wearing the span marker, and that the
// refusal leaves the chain unchanged and the legitimate beat path working. Authentication stores
// token names verbatim and a request's method and path reach the trail as given, so without this a
// caller holding a token named for the span actor could inject a beat that renumbers the real ones
// and fails every bundle built over the chain.
func testReservedSpan(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

	forged := &audit.Entry{
		ID: audit.NewID(), At: base,
		Actor: audit.SpanActor, Method: audit.SpanMethod, Path: audit.SpanPath(9, 0, 60),
	}
	if err := store.Append(ctx, forged); !errors.Is(err, audit.ErrReservedSpan) {
		t.Fatalf("Append() of a span marker entry error = %v, want ErrReservedSpan", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 0 {
		t.Fatalf("Chain() len = %d after a refused append, want 0", len(chain))
	}

	beat, err := store.AppendSpanBeat(ctx, base.Add(time.Minute), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() after a refusal error = %v", err)
	}
	checkBeat(t, beat, base.Add(time.Minute), 1, 0, 60)
}

// testNearMissSpan verifies an entry that wears the span actor and method but whose path does not
// round-trip stays an ordinary entry: it appends, it never supplies a beat number, and the next
// real beat counts it as prior history.
func testNearMissSpan(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

	if err := store.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base,
		Actor: audit.SpanActor, Method: audit.SpanMethod, Path: "/span/9?count=0",
	}); err != nil {
		t.Fatalf("Append() of a near-miss entry error = %v, want it to append as ordinary", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("Chain() len = %d, want 1", len(chain))
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d after a near-miss append", at)
	}

	beat, err := store.AppendSpanBeat(ctx, base.Add(time.Minute), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	checkBeat(t, beat, base.Add(time.Minute), 1, 1, 60)
}

// testSpanBeatsQuery verifies SpanBeats answers only well-formed beats, oldest first, and that a
// limit keeps the newest ones without letting a near-miss entry use up a slot.
func testSpanBeatsQuery(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

	appendMutations(t, store, base, 2)
	if _, err := store.AppendSpanBeat(ctx, base.Add(time.Hour), 60); err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	if err := store.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base.Add(90 * time.Minute),
		Actor: audit.SpanActor, Method: audit.SpanMethod, Path: "/span/9?count=0",
	}); err != nil {
		t.Fatalf("Append() of a near-miss entry error = %v", err)
	}
	for i := range 2 {
		at := base.Add(time.Duration(2+i) * time.Hour)
		if _, err := store.AppendSpanBeat(ctx, at, 60); err != nil {
			t.Fatalf("AppendSpanBeat() error = %v", err)
		}
	}

	all, err := store.SpanBeats(ctx, 10)
	if err != nil {
		t.Fatalf("SpanBeats() error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("SpanBeats(10) len = %d, want 3 with the near-miss excluded", len(all))
	}
	for i, e := range all {
		beat, _, _, ok := audit.ParseSpanPath(e.Path)
		if !ok || !audit.IsSpanMarker(e) {
			t.Fatalf("SpanBeats(10)[%d] = %+v, want only well-formed beats", i, e)
		}
		if beat != int64(i+1) {
			t.Errorf("SpanBeats(10)[%d] beat = %d, want %d oldest first", i, beat, i+1)
		}
		if i > 0 && all[i-1].Seq >= e.Seq {
			t.Errorf("SpanBeats(10)[%d] seq = %d after %d, want ascending", i, e.Seq, all[i-1].Seq)
		}
	}

	// A limit keeps the newest beats, still oldest first within the answer, and the near-miss row
	// between beats one and two must not consume a slot.
	capped, err := store.SpanBeats(ctx, 2)
	if err != nil {
		t.Fatalf("SpanBeats(2) error = %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("SpanBeats(2) len = %d, want 2", len(capped))
	}
	for i, e := range capped {
		if beat, _, _, _ := audit.ParseSpanPath(e.Path); beat != int64(i+2) {
			t.Errorf("SpanBeats(2)[%d] beat = %d, want %d: the newest two, oldest first", i, beat, i+2)
		}
	}

	// Asking for three must reach past the near-miss row to beat one. A store that budgets its scan
	// by span-marked rows rather than by well-formed beats comes up one short here.
	three, err := store.SpanBeats(ctx, 3)
	if err != nil {
		t.Fatalf("SpanBeats(3) error = %v", err)
	}
	if len(three) != 3 {
		t.Fatalf("SpanBeats(3) len = %d, want 3: the near-miss must not use up a slot", len(three))
	}
}

// testConcurrentAppend fires many appends at once and checks the chain stays a single intact line:
// contiguous unique sequences and a passing Verify. A store that reads its head and inserts without
// serializing forks here, showing up as a duplicate sequence or a broken chain.
func testConcurrentAppend(t *testing.T, store audit.Store) {
	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.Append(ctx, &audit.Entry{
				ID: audit.NewID(), At: time.Unix(int64(i), 0).UTC(),
				Actor: "root", Method: "POST", Path: "/runs",
			}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append() error = %v", err)
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != n {
		t.Fatalf("Chain() len = %d, want %d", len(chain), n)
	}
	seen := make(map[int64]bool, n)
	for i, e := range chain {
		if e.Seq != int64(i+1) {
			t.Errorf("entry %d seq = %d, want %d", i, e.Seq, i+1)
		}
		if seen[e.Seq] {
			t.Errorf("duplicate seq %d: the chain forked", e.Seq)
		}
		seen[e.Seq] = true
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d after concurrent appends", at)
	}
}

// testChain verifies that appended entries form an intact hash chain with contiguous sequences that
// survives a store round-trip.
func testChain(t *testing.T, store audit.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for i, path := range []string{"/runs", "/projects", "/users", "/schedules"} {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "root", Method: "POST", Path: path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 4 {
		t.Fatalf("Chain() len = %d, want 4", len(chain))
	}
	for i, e := range chain {
		if e.Seq != int64(i+1) {
			t.Errorf("entry %d seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Hash == "" {
			t.Errorf("entry %d has no hash", i)
		}
	}
	if chain[0].PrevHash != "" {
		t.Errorf("first entry prev_hash = %q, want empty", chain[0].PrevHash)
	}
	if chain[1].PrevHash != chain[0].Hash {
		t.Errorf("second entry prev_hash = %q, want the first hash %q", chain[1].PrevHash, chain[0].Hash)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d, want an intact chain", at)
	}

	// ChainScan streams the same entries in the same order Chain returns them, and a scanner fed
	// from it reaches the same verdict.
	var scanned []*audit.Entry
	if err := store.ChainScan(ctx, 0, func(e *audit.Entry) error {
		scanned = append(scanned, e)
		return nil
	}); err != nil {
		t.Fatalf("ChainScan() error = %v", err)
	}
	if diff := cmp.Diff(chain, scanned); diff != "" {
		t.Errorf("ChainScan() mismatch with Chain() (-chain +scanned):\n%s", diff)
	}
	if ok, at := audit.Verify(scanned); !ok {
		t.Errorf("Verify() over the scan reported a break at %d, want an intact chain", at)
	}

	// An error from fn stops the scan and comes back verbatim.
	stop := errors.New("stop here")
	calls := 0
	err = store.ChainScan(ctx, 0, func(*audit.Entry) error {
		calls++
		if calls == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Errorf("ChainScan() error = %v, want the fn error back", err)
	}

	// A cursor resumes exactly after the given sequence: no repeats, no gaps. An incremental
	// reader that re-fed a boundary entry would break its own scanner's continuity check.
	var resumed []*audit.Entry
	if err := store.ChainScan(ctx, chain[0].Seq, func(e *audit.Entry) error {
		resumed = append(resumed, e)
		return nil
	}); err != nil {
		t.Fatalf("ChainScan(afterSeq) error = %v", err)
	}
	if diff := cmp.Diff(chain[1:], resumed); diff != "" {
		t.Errorf("ChainScan(afterSeq) mismatch (-want +got):\n%s", diff)
	}
	if err := store.ChainScan(ctx, chain[len(chain)-1].Seq, func(e *audit.Entry) error {
		t.Errorf("ChainScan() past the head streamed seq %d, want nothing", e.Seq)
		return nil
	}); err != nil {
		t.Fatalf("ChainScan(head) error = %v", err)
	}
	if calls != 2 {
		t.Errorf("ChainScan() called fn %d times after a stop at 2", calls)
	}
}

// testAppendList verifies entries come back newest first with the limit honored.
func testAppendList(t *testing.T, store audit.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for i, path := range []string{"/runs", "/projects", "/users"} {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "root", Method: "POST", Path: path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	all, err := store.List(ctx, 10)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 3 || all[0].Path != "/users" || all[2].Path != "/runs" {
		t.Errorf("List() = %+v, want newest first users..runs", all)
	}
	if all[0].Actor != "root" || all[0].Method != "POST" {
		t.Errorf("entry = %+v, want actor root method POST", all[0])
	}

	one, err := store.List(ctx, 1)
	if err != nil {
		t.Fatalf("List(1) error = %v", err)
	}
	if len(one) != 1 || one[0].Path != "/users" {
		t.Errorf("List(1) = %+v, want just the newest", one)
	}
}

// testEntryByID pins the primary-key read an executor starts every binding check from: each
// appended entry comes back by its id exactly as the chain holds it, and an id the chain does not
// hold is reported with audit.ErrEntryNotFound rather than as an empty entry.
func testEntryByID(t *testing.T, store audit.Store) {
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	ids := make([]string, 0, 3)
	for i, path := range []string{"/runs", "/projects", "/users"} {
		digest, nonce, err := audit.ContentDigestOf([]byte(`{"step":"` + path + `"}`))
		if err != nil {
			t.Fatalf("ContentDigestOf() error = %v", err)
		}
		e := &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "root", ActorType: "user", Method: "POST", Path: path,
			ContentDigest: digest, Nonce: nonce,
		}
		if err := store.Append(ctx, e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		ids = append(ids, e.ID)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	held := map[string]*audit.Entry{}
	for _, e := range chain {
		held[e.ID] = e
	}

	tests := []struct {
		// ID is the id read.
		ID string
		// WantEntry is the entry the read must return, nil when none.
		WantEntry *audit.Entry
		// Want is the error the read must return.
		Want error
	}{{ // Test 0: The first entry appended.
		ID: ids[0], WantEntry: held[ids[0]], Want: nil,
	}, { // Test 1: An entry in the middle of the chain.
		ID: ids[1], WantEntry: held[ids[1]], Want: nil,
	}, { // Test 2: The chain head.
		ID: ids[2], WantEntry: held[ids[2]], Want: nil,
	}, { // Test 3: An id the chain never held.
		ID: "aud_missing", WantEntry: nil, Want: audit.ErrEntryNotFound,
	}, { // Test 4: The empty id.
		ID: "", WantEntry: nil, Want: audit.ErrEntryNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got, err := store.Entry(ctx, test.ID)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Entry(%q) error = %v, want %v", test.ID, err, test.Want)
			}
			if diff := cmp.Diff(test.WantEntry, got); diff != "" {
				t.Errorf("Entry(%q) mismatch (-want +got):\n%s", test.ID, diff)
			}
		})
	}
}

// testAnchors verifies that anchors persist, come back oldest first, and scope to a sequence.
//
// An anchor fixes a chain link somewhere the operator cannot rewrite alone, which is what turns "the
// chain has not been altered" into "the chain has also not been shortened". That only holds if the
// anchor itself survives, so it is stored beside the entries rather than left in a log line, and a
// bundle asks for the anchors covering the range it carries rather than all of them.
//
// Identifiers are generated rather than fixed because a contract store may be shared across cases,
// so the assertions are about the anchors this case saved rather than about absolute counts.
func testAnchors(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	anchors, ok := store.(audit.AnchorStore)
	if !ok {
		t.Fatalf("%T does not persist anchors, so a chain it holds cannot be fixed in time", store)
	}
	before, err := anchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}

	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	stamped := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorRFC3161, Shape: audit.AnchorShapeLinear,
		Seq: 5, Link: "aa", At: at, Ref: "https://freetsa.org/tsr", Proof: "MIIBase64Token",
	}
	committed := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorGit, Shape: audit.AnchorShapeTree,
		Seq: 20, Link: "bb", At: at.Add(time.Hour),
		Ref: "https://github.com/acme/anchors/commit/deadbeef",
	}
	for _, a := range []*audit.Anchor{stamped, committed} {
		if err := anchors.SaveAnchor(ctx, a); err != nil {
			t.Fatalf("SaveAnchor(%s) error = %v", a.ID, err)
		}
	}

	all, err := anchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors(0) error = %v", err)
	}
	if len(all) != len(before)+2 {
		t.Fatalf("Anchors(0) returned %d, want %d", len(all), len(before)+2)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Seq > all[i].Seq {
			t.Errorf("anchors came back out of order at index %d, want oldest first", i)
		}
	}

	byID := make(map[string]*audit.Anchor, len(all))
	for _, a := range all {
		byID[a.ID] = a
	}
	got, ok := byID[stamped.ID]
	if !ok {
		t.Fatal("the saved rfc3161 anchor did not come back")
	}
	// The embedded proof is the whole value of an rfc3161 anchor. Losing it in storage would leave a
	// record that looks anchored and proves nothing.
	if got.Proof != stamped.Proof {
		t.Errorf("proof = %q, want it stored verbatim", got.Proof)
	}
	if got.Ref != stamped.Ref || got.Type != audit.AnchorRFC3161 || got.Link != "aa" {
		t.Errorf("anchor = %+v, want its type, link, and reference preserved", got)
	}
	if !got.At.Equal(at) {
		t.Errorf("anchor time = %s, want %s", got.At, at)
	}
	// The shape decides how an anchor is checked, so losing it in storage turns a tree anchor into
	// one held against the linear hash map, the exact confusion the field exists to prevent.
	if got.Shape != audit.AnchorShapeLinear {
		t.Errorf("shape = %q, want %q stored verbatim", got.Shape, audit.AnchorShapeLinear)
	}
	if tree := byID[committed.ID]; tree == nil || tree.Shape != audit.AnchorShapeTree {
		t.Errorf("tree anchor shape did not round trip: %+v", tree)
	}

	// A bundle covering the first ten entries must not carry an anchor for entry twenty: a verifier
	// rejects a bundle whose anchor names a link it does not hold.
	scoped, err := anchors.Anchors(ctx, 10)
	if err != nil {
		t.Fatalf("Anchors(10) error = %v", err)
	}
	found := false
	for _, a := range scoped {
		if a.Seq > 10 {
			t.Errorf("Anchors(10) returned an anchor at seq %d, which names a link a bundle of the "+
				"first ten entries does not hold", a.Seq)
		}
		if a.ID == stamped.ID {
			found = true
		}
		if a.ID == committed.ID {
			t.Error("Anchors(10) returned the anchor at seq 20")
		}
	}
	if !found {
		t.Error("Anchors(10) dropped the anchor at seq 5, which is inside the range")
	}
}

// testAnchorDelete verifies an anchor can be withdrawn by id: the named anchor is gone from the
// listing, the others survive, and deleting a missing id reports audit.ErrAnchorNotFound. Without
// a delete path, an anchor recorded over the wrong coordinates fails every export forever.
func testAnchorDelete(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	anchors, ok := store.(audit.AnchorStore)
	if !ok {
		t.Fatalf("%T does not persist anchors", store)
	}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	doomed := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeTree,
		Seq: 7, Link: "cc", At: at, Ref: "https://anchors.example/head",
	}
	kept := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeLinear,
		Seq: 3, Link: "dd", At: at, Ref: "https://anchors.example/head",
	}
	for _, a := range []*audit.Anchor{doomed, kept} {
		if err := anchors.SaveAnchor(ctx, a); err != nil {
			t.Fatalf("SaveAnchor(%s) error = %v", a.ID, err)
		}
	}

	if err := anchors.DeleteAnchor(ctx, doomed.ID); err != nil {
		t.Fatalf("DeleteAnchor(%s) error = %v", doomed.ID, err)
	}
	after, err := anchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}
	keptSeen := false
	for _, a := range after {
		if a.ID == doomed.ID {
			t.Error("the deleted anchor is still listed")
		}
		if a.ID == kept.ID {
			keptSeen = true
		}
	}
	if !keptSeen {
		t.Error("deleting one anchor removed another")
	}

	if err := anchors.DeleteAnchor(ctx, doomed.ID); !errors.Is(err, audit.ErrAnchorNotFound) {
		t.Errorf("DeleteAnchor() of a missing id error = %v, want audit.ErrAnchorNotFound", err)
	}
}

// attestationStore returns the witness attestation half of store, failing the test when it has
// none.
func attestationStore(t *testing.T, store audit.Store) audit.AttestationStore {
	t.Helper()
	atts, ok := store.(audit.AttestationStore)
	if !ok {
		t.Fatalf("%T does not persist witness attestations, so a countersigned beat leaves no "+
			"record a bundle can carry", store)
	}
	return atts
}

// witnessKeyID returns a fresh witness key id. Each case signs under a key of its own because a
// contract store may be shared across cases and runs, and the store holds one record per seq, kind,
// and key, so a fixed key would collide with what an earlier case left behind.
func witnessKeyID() string { return idgen.New("sha256:", 32) }

// newWitnessAttestation returns an attestation of kind at seq by the witness key keyID with every
// member filled, so a store that drops or rewrites any one of them fails the round trip. The time
// carries nanoseconds because the signature covers the exact string, so a store that parsed and
// reformatted it would break every signature whose time it shortened.
func newWitnessAttestation(kind string, seq int64, keyID string) *audit.WitnessAttestation {
	a := &audit.WitnessAttestation{
		ID: audit.NewAttestationID(), Seq: seq, Kind: kind, KeyID: keyID,
		PublicKey: "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=", Role: "witness",
		At: "2026-10-21T12:00:00.123456789Z", Sig: "c2lnbmVkIGJ5IHRoZSB3aXRuZXNz" + keyID[7:15],
		Witness: keyID,
	}
	if kind == audit.AttestationHead {
		a.Size, a.Root = seq, fmt.Sprintf("%064x", seq)
	} else {
		a.Link = fmt.Sprintf("%064x", seq+1)
	}
	return a
}

// ownAttestations lists every attestation at or below seq and keeps those signed by keyID, the
// records this case saved, in the order the store returned them. It fails the test when the full
// listing is not oldest first with ties broken on id.
func ownAttestations(t *testing.T, atts audit.AttestationStore, seq int64, keyID string) []*audit.WitnessAttestation {
	t.Helper()
	all, err := atts.Attestations(context.Background(), seq)
	if err != nil {
		t.Fatalf("Attestations(%d) error = %v", seq, err)
	}
	if all == nil {
		t.Errorf("Attestations(%d) = nil, want a non-nil slice", seq)
	}
	out := []*audit.WitnessAttestation{}
	for i, a := range all {
		if seq > 0 && a.Seq > seq {
			t.Errorf("Attestations(%d) returned one at seq %d, which names a coordinate a bundle "+
				"ending at %d does not hold", seq, a.Seq, seq)
		}
		if i > 0 {
			prev := all[i-1]
			if prev.Seq > a.Seq || (prev.Seq == a.Seq && prev.ID > a.ID) {
				t.Errorf("Attestations(%d) out of order at index %d: %d %s after %d %s, want oldest "+
					"first with ties on id", seq, i, a.Seq, a.ID, prev.Seq, prev.ID)
			}
		}
		if a.KeyID == keyID {
			out = append(out, a)
		}
	}
	return out
}

// testAttestations verifies witness attestations persist every member verbatim, come back oldest
// first, and scope to a sequence the way anchors do.
//
// A witness attestation is the outside party's signature over a link or a tree head, and it is only
// worth keeping if a bundle can later carry it and a verifier can check it. So the members the
// signature covers must come back exactly as stored, and a bundle asks for the attestations
// covering the range it holds rather than all of them, since one naming a coordinate past its end
// refers to something the bundle does not carry.
func testAttestations(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	atts := attestationStore(t, store)
	key := witnessKeyID()
	claim := newWitnessAttestation(audit.AttestationClaim, 5, key)
	// A claim and a head attestation by one key at one seq are two records.
	headAtFive := newWitnessAttestation(audit.AttestationHead, 5, key)
	head := newWitnessAttestation(audit.AttestationHead, 20, key)
	// Signed by a key other than the pinned one, as a witness key rotation leaves it, so a store
	// that conflated the two key ids would fail the round trip.
	head.Witness = witnessKeyID()
	for _, a := range []*audit.WitnessAttestation{head, claim, headAtFive} {
		if err := atts.SaveAttestation(ctx, a); err != nil {
			t.Fatalf("SaveAttestation(%s) error = %v", a.ID, err)
		}
	}
	atFive := []string{claim.ID, headAtFive.ID}
	if atFive[0] > atFive[1] {
		atFive[0], atFive[1] = atFive[1], atFive[0]
	}

	all := ownAttestations(t, atts, 0, key)
	byID := make(map[string]*audit.WitnessAttestation, len(all))
	for _, a := range all {
		byID[a.ID] = a
	}
	for _, want := range []*audit.WitnessAttestation{claim, headAtFive, head} {
		if diff := cmp.Diff(want, byID[want.ID]); diff != "" {
			t.Errorf("attestation %s %s did not round trip (-want +got):\n%s", want.Kind, want.ID,
				diff)
		}
	}

	tests := []struct {
		// Seq is the sequence the listing is scoped to.
		Seq int64
		// WantIDs is the ids of this case's attestations the listing must return, in order.
		WantIDs []string
	}{{ // Test 0: Zero returns every attestation, oldest first with ties on id.
		Seq: 0, WantIDs: []string{atFive[0], atFive[1], head.ID},
	}, { // Test 1: A range ending at ten holds both records at five and not the head at twenty.
		Seq: 10, WantIDs: []string{atFive[0], atFive[1]},
	}, { // Test 2: A range ending exactly at twenty holds the head at twenty.
		Seq: 20, WantIDs: []string{atFive[0], atFive[1], head.ID},
	}, { // Test 3: A range ending at four holds none of them.
		Seq: 4, WantIDs: []string{},
	}, { // Test 4: A negative sequence returns every attestation, as zero does.
		Seq: -1, WantIDs: []string{atFive[0], atFive[1], head.ID},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got := []string{}
			for _, a := range ownAttestations(t, atts, test.Seq, key) {
				got = append(got, a.ID)
			}
			if diff := cmp.Diff(test.WantIDs, got); diff != "" {
				t.Errorf("Attestations(%d) mismatch (-want +got):\n%s", test.Seq, diff)
			}
		})
	}
}

// testAttestationDelete verifies a witness attestation can be withdrawn by id: the named one is
// gone from the listing, the others survive, its coordinate is free to record again, and deleting
// a missing id reports audit.ErrAttestationNotFound.
func testAttestationDelete(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	atts := attestationStore(t, store)
	key := witnessKeyID()
	doomed := newWitnessAttestation(audit.AttestationClaim, 7, key)
	kept := newWitnessAttestation(audit.AttestationClaim, 3, key)
	for _, a := range []*audit.WitnessAttestation{doomed, kept} {
		if err := atts.SaveAttestation(ctx, a); err != nil {
			t.Fatalf("SaveAttestation(%s) error = %v", a.ID, err)
		}
	}

	if err := atts.DeleteAttestation(ctx, doomed.ID); err != nil {
		t.Fatalf("DeleteAttestation(%s) error = %v", doomed.ID, err)
	}
	got := []string{}
	for _, a := range ownAttestations(t, atts, 0, key) {
		got = append(got, a.ID)
	}
	if diff := cmp.Diff([]string{kept.ID}, got); diff != "" {
		t.Errorf("after deleting %s (-want +got):\n%s", doomed.ID, diff)
	}

	if err := atts.DeleteAttestation(ctx, doomed.ID); !errors.Is(err, audit.ErrAttestationNotFound) {
		t.Errorf("DeleteAttestation() of a missing id error = %v, want "+
			"audit.ErrAttestationNotFound", err)
	}

	// A withdrawn record frees its coordinate, so a correct one can take its place.
	again := newWitnessAttestation(audit.AttestationClaim, 7, key)
	again.Sig = "cmVwbGFjZW1lbnQ="
	if err := atts.SaveAttestation(ctx, again); err != nil {
		t.Fatalf("SaveAttestation() into a withdrawn coordinate error = %v", err)
	}
	if again.ID == doomed.ID {
		t.Error("the new record took the withdrawn record's id")
	}
}

// testAttestationHeld verifies the first attestation written at a seq, kind, and witness key wins
// and never changes. The same answer saved again succeeds under the held id, which is what a
// retried push produces, and a different answer at the same coordinate is refused with
// audit.ErrAttestationConflict, which is what a restored witness re-minting at a seq it already
// countersigned produces. Overwriting would let the later answer silently replace the one bundles
// may already carry.
func testAttestationHeld(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	atts := attestationStore(t, store)
	key := witnessKeyID()
	held := newWitnessAttestation(audit.AttestationClaim, 9, key)
	if err := atts.SaveAttestation(ctx, held); err != nil {
		t.Fatalf("SaveAttestation() error = %v", err)
	}
	want := *held

	// variant returns a copy of the held record under a fresh id with edit applied.
	variant := func(edit func(*audit.WitnessAttestation)) *audit.WitnessAttestation {
		cp := want
		cp.ID = audit.NewAttestationID()
		edit(&cp)
		return &cp
	}
	same := want
	tests := []struct {
		// Record is the attestation saved.
		Record *audit.WitnessAttestation
		// WantID is the id the saved record carries after the save.
		WantID string
		// Want is the error the save must return.
		Want error
	}{{ // Test 0: The same answer under a fresh id succeeds and takes the held id.
		Record: variant(func(*audit.WitnessAttestation) {}), WantID: want.ID, Want: nil,
	}, { // Test 1: The held record itself saved again succeeds unchanged.
		Record: &same, WantID: want.ID, Want: nil,
	}, { // Test 2: A re-mint with a new time and signature is refused.
		Record: variant(func(a *audit.WitnessAttestation) {
			a.At, a.Sig = "2026-10-21T13:00:00Z", "cmUtbWludGVk"
		}),
		Want: audit.ErrAttestationConflict,
	}, { // Test 3: Another link at the same seq is refused.
		Record: variant(func(a *audit.WitnessAttestation) { a.Link = fmt.Sprintf("%064x", 99) }),
		Want:   audit.ErrAttestationConflict,
	}, { // Test 4: The same countersignature under another pinned witness is refused.
		Record: variant(func(a *audit.WitnessAttestation) { a.Witness = witnessKeyID() }),
		Want:   audit.ErrAttestationConflict,
	}, { // Test 5: Another witness key at the same seq and kind is its own record.
		Record: variant(func(a *audit.WitnessAttestation) { a.KeyID = witnessKeyID() }),
		Want:   nil,
	}, { // Test 6: A head attestation at the same seq by the same key is its own record.
		Record: variant(func(a *audit.WitnessAttestation) {
			a.Kind, a.Link, a.Size, a.Root = audit.AttestationHead, "", a.Seq, fmt.Sprintf("%064x", 9)
		}),
		Want: nil,
	}, { // Test 7: The same key and kind at the next seq is its own record.
		Record: variant(func(a *audit.WitnessAttestation) { a.Seq++ }),
		Want:   nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			wantID := test.WantID
			if wantID == "" {
				wantID = test.Record.ID
			}
			err := atts.SaveAttestation(ctx, test.Record)
			if !errors.Is(err, test.Want) {
				t.Fatalf("SaveAttestation() error = %v, want %v", err, test.Want)
			}
			if test.Record.ID != wantID {
				t.Errorf("saved record id = %s, want %s", test.Record.ID, wantID)
			}
		})
	}

	got := []*audit.WitnessAttestation{}
	for _, a := range ownAttestations(t, atts, want.Seq, key) {
		if a.Seq == want.Seq && a.Kind == want.Kind {
			got = append(got, a)
		}
	}
	if diff := cmp.Diff([]*audit.WitnessAttestation{&want}, got); diff != "" {
		t.Errorf("the record held at seq %d changed (-want +got):\n%s", want.Seq, diff)
	}
}

// testAttestationConcurrent verifies the first-write rule holds under concurrent saves of one
// coordinate. Saves of the same answer all succeed and all name the one record kept. Saves of
// different answers leave exactly one winner, every other save refused with
// audit.ErrAttestationConflict, and the record kept is the winner's.
func testAttestationConcurrent(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	atts := attestationStore(t, store)
	const writers = 8

	sameKey := witnessKeyID()
	base := newWitnessAttestation(audit.AttestationClaim, 11, sameKey)
	retries := make([]*audit.WitnessAttestation, writers)
	for i := range retries {
		cp := *base
		cp.ID = audit.NewAttestationID()
		retries[i] = &cp
	}
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range retries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = atts.SaveAttestation(ctx, retries[i])
		}(i)
	}
	wg.Wait()
	kept := ownAttestations(t, atts, 0, sameKey)
	if len(kept) != 1 {
		t.Fatalf("%d concurrent saves of one answer kept %d records, want 1", writers, len(kept))
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("save %d of the same answer error = %v, want nil", i, err)
		}
		if retries[i].ID != kept[0].ID {
			t.Errorf("save %d names %s, want the kept record %s", i, retries[i].ID, kept[0].ID)
		}
	}

	rivalKey := witnessKeyID()
	rivals := make([]*audit.WitnessAttestation, writers)
	for i := range rivals {
		rivals[i] = newWitnessAttestation(audit.AttestationClaim, 11, rivalKey)
		rivals[i].Sig = fmt.Sprintf("cml2YWwg%04d", i)
	}
	for i := range rivals {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = atts.SaveAttestation(ctx, rivals[i])
		}(i)
	}
	wg.Wait()
	var winner *audit.WitnessAttestation
	for i, err := range errs {
		switch {
		case err == nil && winner != nil:
			t.Errorf("rival saves %s and %s both succeeded, want exactly one", winner.ID,
				rivals[i].ID)
		case err == nil:
			winner = rivals[i]
		case !errors.Is(err, audit.ErrAttestationConflict):
			t.Errorf("rival save %d error = %v, want audit.ErrAttestationConflict", i, err)
		}
	}
	if winner == nil {
		t.Fatal("no rival save succeeded, want exactly one")
	}
	if diff := cmp.Diff([]*audit.WitnessAttestation{winner},
		ownAttestations(t, atts, 0, rivalKey)); diff != "" {
		t.Errorf("the kept rival is not the winner's record (-want +got):\n%s", diff)
	}
}

// testCommittedFields proves the fields added beyond the original six survive a store round trip and
// are committed by the chain link.
//
// The forgery attempt is the point. An entry is stored, then its content digest is altered while
// every field the old construction hashed is left exactly as it was: same sequence, same time, same
// actor, same method, same path, same previous link. Under the old positional construction the
// recomputed hash matched and the tampered entry verified, so the chain proved a call had been made
// and said nothing about what the call contained. Under the current one the hash no longer matches
// and the chain reports the break, which is what makes "the record covers the change, not only the
// request" a true statement rather than a marketing one.
func testCommittedFields(t *testing.T, store audit.Store) {
	ctx := context.Background()
	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	digest, nonce, err := audit.ContentDigestOf([]byte(`{"limit":"web01"}`))
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "agent", ActorType: "token",
		OnBehalfOf: "deploy-agent", Method: "POST", Path: "/v1/templates/tpl_1/launch",
		ContentDigest: digest, Nonce: nonce,
	}
	if err := store.Append(ctx, entry); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("chain = %d entries, want 1", len(chain))
	}
	got := chain[0]
	// The fields must come back from the store, or the entry that was hashed is not the entry stored.
	for _, field := range []struct{ Name, Got, Want string }{
		{"actor_type", got.ActorType, "token"},
		{"on_behalf_of", got.OnBehalfOf, "deploy-agent"},
		{"content_digest", got.ContentDigest, entry.ContentDigest},
		{"nonce", got.Nonce, entry.Nonce},
	} {
		if field.Got != field.Want {
			t.Errorf("%s did not round trip: got %q, want %q", field.Name, field.Got, field.Want)
		}
	}
	if ok, _ := audit.Verify(chain); !ok {
		t.Fatal("the stored chain does not verify")
	}

	// The forgery: change what the call contained, leave everything the old link hashed untouched.
	otherDigest, _, err := audit.ContentDigestOf([]byte(`{"limit":"*"}`))
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	forged := *got
	forged.ContentDigest = otherDigest
	if forged.ContentDigest == got.ContentDigest {
		t.Fatal("the two payloads digest alike, so this proves nothing")
	}
	if audit.EntryHash(&forged) == got.Hash {
		t.Error("the content digest is not committed by the link: a change to the payload the entry " +
			"records leaves its hash intact, so the chain proves only that a call was made")
	}
	// The same for the delegation, which is what attributes an agent's work to the authority it used.
	forgedActor := *got
	forgedActor.OnBehalfOf = "someone-else"
	if audit.EntryHash(&forgedActor) == got.Hash {
		t.Error("on_behalf_of is not committed by the link, so a recorded delegation can be rewritten")
	}
	forgedType := *got
	forgedType.ActorType = "session"
	if audit.EntryHash(&forgedType) == got.Hash {
		t.Error("actor_type is not committed by the link, so an agent's change can be presented as a " +
			"person's")
	}
}

// testInstallBinding pins that binding an install both reaches storage and leaves the chain
// verifiable, on every backend.
//
// The install id is folded into the chain link, so the read path has to return it. When it was
// added to the hash without a column to hold it, every entry hashed one way on the way in and
// another on the way back, and the whole chain reported broken at entry one. Nothing failed: the
// binding is set only by the serving command, so no test ever set it and every store test ran with
// it empty. This is that test.
func testInstallBinding(t *testing.T, store audit.Store) {
	t.Helper()
	binder, ok := store.(audit.InstallBinder)
	if !ok {
		t.Fatalf("%T does not bind an install, so its chain links cannot name one", store)
	}
	const installID = "in_0123456789ab"
	binder.BindInstall(installID)

	ctx := context.Background()
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		e := &audit.Entry{
			ID: fmt.Sprintf("aud_bind%d", i), At: at.Add(time.Duration(i) * time.Second),
			Actor: "release-token", ActorType: "token", Method: "POST", Path: "/v1/runs",
		}
		if err := store.Append(ctx, e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		if e.InstallID != installID {
			t.Fatalf("appended entry install_id = %q, want %q", e.InstallID, installID)
		}
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3", len(chain))
	}
	for _, e := range chain {
		// Read back from storage, not the struct that was written, since the whole failure was a
		// value that survived the write and vanished on the way out.
		if e.InstallID != installID {
			t.Errorf("entry %d read back with install_id %q, want %q", e.Seq, e.InstallID, installID)
		}
	}
	if ok, brokeAt := audit.Verify(chain); !ok {
		t.Fatalf("a chain written by a bound install does not verify, broke at %d. The install id "+
			"is hashed into every link, so a read path that cannot return it breaks the chain.",
			brokeAt)
	}
}

// testBoundInstall pins what a store says about the install its chain belongs to. A process that
// finds its signing key missing has to tell a first start, where minting one is right, from a chain
// another key already claimed, where minting one silently starts a second install whose bundles can
// never verify. An unbound entry written after the binding, such as one from a process with no key,
// must not hide the install the chain was bound to.
func testBoundInstall(t *testing.T, store audit.Store) {
	t.Helper()
	reader, ok := store.(audit.InstallReader)
	if !ok {
		t.Fatalf("%T cannot say which install its chain belongs to", store)
	}
	binder, ok := store.(audit.InstallBinder)
	if !ok {
		t.Fatalf("%T does not bind an install", store)
	}
	ctx := context.Background()
	appendOne := func(id string) {
		t.Helper()
		e := &audit.Entry{ID: id, Actor: "ops", ActorType: "token", Method: "POST", Path: "/v1/runs"}
		if err := store.Append(ctx, e); err != nil {
			t.Fatalf("Append(%s) error = %v", id, err)
		}
	}
	want := func(label, wantID string) {
		t.Helper()
		got, err := reader.BoundInstall(ctx)
		if err != nil {
			t.Fatalf("%s: BoundInstall() error = %v", label, err)
		}
		if got != wantID {
			t.Errorf("%s: BoundInstall() = %q, want %q", label, got, wantID)
		}
	}
	want("an empty chain", "")
	appendOne("aud_unbound")
	want("a chain with only unbound entries", "")
	binder.BindInstall("in_first")
	appendOne("aud_first")
	want("after a bound entry", "in_first")
	binder.BindInstall("")
	appendOne("aud_after")
	want("after an unbound entry follows a bound one", "in_first")
	binder.BindInstall("in_second")
	appendOne("aud_second")
	want("after a newer bound entry", "in_second")
}

// testAppendPinsBehindClock proves the store never records a chain whose times invert, even when
// the caller chose the time. Two servers on one chain stamp their own wall clocks: a decision
// committed on a fast replica followed by an outcome committed on a slow one arrived with the
// outcome's chosen time BEHIND the decision's, and a verifier reading recorded times then reports
// the approval as postdating the run it released, a bypassed gate, permanently, over an honest
// approval. The append pins a behind-the-head time forward to the head's; an ahead time is kept.
func testAppendPinsBehindClock(t *testing.T, store audit.Store) {
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	decision := &audit.Entry{ID: audit.NewID(), At: base.Add(2 * time.Second),
		Actor: "approver", ActorType: "user", Method: audit.MethodDecision,
		Path: "/runs/run_skew/decision/approved"}
	if err := store.Append(ctx, decision); err != nil {
		t.Fatalf("append decision: %v", err)
	}
	outcome := &audit.Entry{ID: audit.NewID(), At: base,
		Actor: "system:dispatcher", ActorType: "system", Method: audit.MethodRun,
		Path: "/runs/run_skew/outcome/succeeded"}
	if err := store.Append(ctx, outcome); err != nil {
		t.Fatalf("append outcome: %v", err)
	}

	entries, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("chain holds %d entries, want 2", len(entries))
	}
	got := entries[len(entries)-1]
	dec := entries[len(entries)-2]
	if got.At.Before(dec.At) {
		t.Fatalf("outcome recorded at %s, before the decision at %s: replica skew inverted the "+
			"chain and every receipt over this pair reports the gate as bypassed", got.At, dec.At)
	}
	if ok, at := audit.Verify(entries); !ok {
		t.Fatalf("chain does not verify after the pin (position %d)", at)
	}

	ahead := &audit.Entry{ID: audit.NewID(), At: base.Add(time.Minute),
		Actor: "approver", ActorType: "user", Method: audit.MethodDecision,
		Path: "/runs/run_skew2/decision/approved"}
	if err := store.Append(ctx, ahead); err != nil {
		t.Fatalf("append ahead: %v", err)
	}
	entries, err = store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if last := entries[len(entries)-1]; !last.At.Equal(base.Add(time.Minute)) {
		t.Fatalf("a chosen time ahead of the head was altered to %s", last.At)
	}
}
