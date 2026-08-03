// Package audittest provides a shared behavior contract for audit.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package audittest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// Contract runs the audit.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() audit.Store) {
	t.Helper()
	t.Run("append and list", func(t *testing.T) { testAppendList(t, newStore()) })
	t.Run("chain verifies", func(t *testing.T) { testChain(t, newStore()) })
	t.Run("anchors round trip and scope", func(t *testing.T) { testAnchors(t, newStore()) })
	t.Run("concurrent appends do not fork", func(t *testing.T) { testConcurrentAppend(t, newStore()) })
	t.Run("span beats increment with counts", func(t *testing.T) { testSpanBeats(t, newStore()) })
	t.Run("span beat one adopts prior history", func(t *testing.T) { testSpanAdoption(t, newStore()) })
	t.Run("concurrent span beats never collide", func(t *testing.T) {
		testConcurrentSpanBeats(t, newStore())
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
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.AppendSpanBeat(ctx, time.Unix(int64(i), 0).UTC(), 60); err != nil {
				errs <- err
			}
		}(i)
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
		ID: audit.NewAnchorID(), Type: audit.AnchorRFC3161, Seq: 5, Link: "aa", At: at,
		Ref: "https://freetsa.org/tsr", Proof: "MIIBase64Token",
	}
	committed := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorGit, Seq: 20, Link: "bb", At: at.Add(time.Hour),
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
