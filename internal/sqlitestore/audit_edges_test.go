package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// auditStores returns the audit store together with the two capabilities the chain depends on but
// that the base interface does not carry, failing the test when either is missing.
func auditStores(t *testing.T, db *sqlitestore.DB) (audit.Store, audit.AnchorStore, audit.InstallBinder) {
	t.Helper()
	s := db.Audits()
	anchors, ok := s.(audit.AnchorStore)
	if !ok {
		t.Fatal("the audit store does not persist anchors, so its chain cannot be fixed in time")
	}
	binder, ok := s.(audit.InstallBinder)
	if !ok {
		t.Fatal("the audit store cannot be bound to an install, so a receipt could be presented " +
			"as another install's history")
	}
	return s, anchors, binder
}

// mutation builds an ordinary chain entry, the shape every recorded change takes.
func mutation(at time.Time, actor, method, path string) *audit.Entry {
	return &audit.Entry{ID: audit.NewID(), At: at, Actor: actor, Method: method, Path: path}
}

// TestAuditReadLimitsClampToAtLeastOneRow pins the floor on the two bounded reads. A limit of zero
// reaching the database as a literal LIMIT 0 returns nothing, so a caller that forgot to set one
// would see an empty trail and read it as an install with no history rather than as a request that
// asked for nothing.
func TestAuditReadLimitsClampToAtLeastOneRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := auditStores(t, openStore(t))

	for i := 0; i < 5; i++ {
		if err := store.Append(ctx, mutation(baseTime.Add(time.Duration(i)*time.Second),
			"sam", "POST", fmt.Sprintf("/v1/runs/%d", i))); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}
	if _, err := store.AppendSpanBeat(ctx, baseTime.Add(time.Hour), 60); err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	if _, err := store.AppendSpanBeat(ctx, baseTime.Add(2*time.Hour), 60); err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}

	tests := []struct {
		Name      string
		Limit     int
		WantCount int
		WantBeats int
	}{{ // Test 0: Zero is clamped to one rather than returning nothing.
		Name: "zero", Limit: 0, WantCount: 1, WantBeats: 1,
	}, { // Test 1: A negative limit is clamped the same way.
		Name: "negative", Limit: -10, WantCount: 1, WantBeats: 1,
	}, { // Test 2: One is one.
		Name: "one", Limit: 1, WantCount: 1, WantBeats: 1,
	}, { // Test 3: A limit larger than the chain returns everything there is, not an error.
		Name: "over", Limit: 1000, WantCount: 7, WantBeats: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			entries, err := store.List(ctx, test.Limit)
			if err != nil {
				t.Fatalf("List(%d) error = %v", test.Limit, err)
			}
			if len(entries) != test.WantCount {
				t.Errorf("List(%d) returned %d entries, want %d", test.Limit, len(entries),
					test.WantCount)
			}
			beats, err := store.SpanBeats(ctx, test.Limit)
			if err != nil {
				t.Fatalf("SpanBeats(%d) error = %v", test.Limit, err)
			}
			if len(beats) != test.WantBeats {
				t.Errorf("SpanBeats(%d) returned %d beats, want %d", test.Limit, len(beats),
					test.WantBeats)
			}
			for _, b := range beats {
				if !audit.IsSpanMarker(b) {
					t.Errorf("SpanBeats returned %+v, which is not a well-formed beat", b)
				}
			}
		})
	}
}

// TestListReturnsTheNewestEntriesNotTheOldest pins which end of the chain a bounded list reads
// from. The audit page shows recent activity, so a list that took the oldest entries would show an
// install's first day forever and never the change somebody is looking for.
func TestListReturnsTheNewestEntriesNotTheOldest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := auditStores(t, openStore(t))

	const total = 10
	for i := 0; i < total; i++ {
		if err := store.Append(ctx, mutation(baseTime.Add(time.Duration(i)*time.Second),
			"sam", "POST", fmt.Sprintf("/v1/runs/%d", i))); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}
	got, err := store.List(ctx, 3)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	wantPaths := []string{"/v1/runs/9", "/v1/runs/8", "/v1/runs/7"}
	paths := make([]string, len(got))
	for i, e := range got {
		paths[i] = e.Path
	}
	if diff := cmp.Diff(wantPaths, paths); diff != "" {
		t.Errorf("List(3) (-newest first +got):\n%s", diff)
	}

	// The chain, by contrast, is oldest first, because that is the order verification needs.
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != total || chain[0].Path != "/v1/runs/0" || chain[0].Seq != 1 {
		t.Errorf("Chain() starts at %+v with %d entries, want genesis first and %d entries",
			chain[0], len(chain), total)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at position %d", at)
	}
}

// TestSpanBeatNumberingIgnoresForgedMarkers pins the defense against an entry that wears the beat
// marker without being one. Every reader of the chain treats a beat as something the server minted,
// so an entry that could supply a beat number would renumber the real ones and fail every bundle
// built over the chain. A well-formed marker is refused at Append outright; a near-miss is accepted
// as an ordinary entry and must then be invisible to the numbering, however many of them there are.
func TestSpanBeatNumberingIgnoresForgedMarkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := auditStores(t, openStore(t))

	// A well-formed marker is refused, so nothing but AppendSpanBeat mints a beat.
	forged := mutation(baseTime, audit.SpanActor, audit.SpanMethod, audit.SpanPath(9999, 0, 60))
	if err := store.Append(ctx, forged); !errors.Is(err, audit.ErrReservedSpan) {
		t.Fatalf("Append(well-formed beat) = %v, want ErrReservedSpan", err)
	}
	if chain, err := store.Chain(ctx); err != nil || len(chain) != 0 {
		t.Fatalf("the refused beat was written anyway: %d entries (%v)", len(chain), err)
	}

	// Near-miss markers are ordinary entries. Several of them, all claiming enormous beat numbers.
	nearMisses := []string{
		"/span/9999?count=0",
		"/span/9999?count=0&cadence_s=0",
		"/span/0?count=0&cadence_s=60",
		"/span/-1?count=0&cadence_s=60",
		"/span/9999?count=-1&cadence_s=60",
		"/span/9999?count=0&cadence_s=60&extra=1",
		"/span/007?count=0&cadence_s=60",
	}
	for i, path := range nearMisses {
		e := mutation(baseTime.Add(time.Duration(i)*time.Second), audit.SpanActor,
			audit.SpanMethod, path)
		if err := store.Append(ctx, e); err != nil {
			t.Fatalf("Append(near miss %q) = %v, want it accepted as an ordinary entry", path, err)
		}
	}

	// The first real beat is beat one and counts every entry before it, exactly as it would on a
	// chain with no forgeries in it at all.
	beat, err := store.AppendSpanBeat(ctx, baseTime.Add(time.Hour), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	number, count, cadence, ok := audit.ParseSpanPath(beat.Path)
	if !ok {
		t.Fatalf("the minted beat's path %q does not round-trip", beat.Path)
	}
	if number != 1 {
		t.Errorf("the first real beat is numbered %d, so a forged marker renumbered the chain",
			number)
	}
	if count != int64(len(nearMisses)) {
		t.Errorf("beat one counts %d entries, want the %d that precede it", count, len(nearMisses))
	}
	if cadence != 60 {
		t.Errorf("cadence = %d, want 60", cadence)
	}

	// The feed shows the real beat only, and the near-misses never take a slot in it.
	beats, err := store.SpanBeats(ctx, 100)
	if err != nil {
		t.Fatalf("SpanBeats() error = %v", err)
	}
	if len(beats) != 1 || beats[0].ID != beat.ID {
		t.Errorf("SpanBeats() = %d entries, want only the one the server minted", len(beats))
	}

	// The next beat follows on from the real one, not from a forged number.
	second, err := store.AppendSpanBeat(ctx, baseTime.Add(2*time.Hour), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat(second) error = %v", err)
	}
	number, _, _, _ = audit.ParseSpanPath(second.Path)
	if number != 2 {
		t.Errorf("the second beat is numbered %d, want 2", number)
	}
}

// TestSpanBeatRefusesAClockThatDidNotAdvance pins the refusal that keeps a false statement out of
// an attestation. A beat's time is a signed claim about when the population was counted, so a clock
// that stepped backward, which an NTP correction or a resumed virtual machine produces, must skip
// the beat rather than mint one carrying a time the clock never read. The number is not consumed,
// so the next beat the chain accepts takes it and the numbering stays contiguous.
func TestSpanBeatRefusesAClockThatDidNotAdvance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := auditStores(t, openStore(t))

	first, err := store.AppendSpanBeat(ctx, baseTime, 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}

	tests := []struct {
		Name string
		At   time.Time
	}{{ // Test 0: A clock that stepped backward.
		Name: "behind", At: baseTime.Add(-time.Hour),
	}, { // Test 1: A clock that has not moved at all.
		Name: "identical", At: baseTime,
	}, { // Test 2: A clock ahead by less than the chain's stored granularity, which is the same
		// instant once written and therefore does not advance.
		Name: "sub granularity", At: baseTime.Add(time.Nanosecond),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got, err := store.AppendSpanBeat(ctx, test.At, 60)
			if !errors.Is(err, audit.ErrClockBehind) {
				t.Fatalf("AppendSpanBeat(%s) = (%v, %v), want ErrClockBehind", test.Name, got, err)
			}
			if got != nil {
				t.Errorf("a refused beat came back as %+v, want nothing", got)
			}
			chain, err := store.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			if len(chain) != 1 {
				t.Errorf("the refused beat was written anyway: %d entries", len(chain))
			}
		})
	}

	// The skipped number waits for the next beat the chain accepts, so the record shows a gap
	// rather than a renumbering.
	next, err := store.AppendSpanBeat(ctx, baseTime.Add(time.Hour), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat(after the skips) error = %v", err)
	}
	firstNumber, _, _, _ := audit.ParseSpanPath(first.Path)
	nextNumber, _, _, _ := audit.ParseSpanPath(next.Path)
	if nextNumber != firstNumber+1 {
		t.Errorf("beat numbering went %d then %d, want contiguous numbering across the skips",
			firstNumber, nextNumber)
	}
}

// TestAnchorScopingIsInclusiveAtTheNamedSequence pins the range an anchor listing answers. Anchors
// are what fix a chain link somewhere this install cannot rewrite alone, and a verifier asks for the
// ones at or below a sequence. An exclusive comparison would drop the anchor sitting exactly at the
// point being checked, which is the one that matters most.
func TestAnchorScopingIsInclusiveAtTheNamedSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, anchors, _ := auditStores(t, openStore(t))

	for _, seq := range []int64{5, 10, 20} {
		if err := anchors.SaveAnchor(ctx, &audit.Anchor{
			ID: fmt.Sprintf("anc_%d", seq), Type: audit.AnchorRFC3161,
			Shape: audit.AnchorShapeLinear, Seq: seq, Link: "link", At: baseTime,
			Ref: "https://freetsa.org/tsr",
		}); err != nil {
			t.Fatalf("SaveAnchor(%d) error = %v", seq, err)
		}
	}

	tests := []struct {
		Name    string
		Seq     int64
		WantIDs []string
	}{{ // Test 0: Zero means no range in mind, so the whole set comes back.
		Name: "zero", Seq: 0, WantIDs: []string{"anc_5", "anc_10", "anc_20"},
	}, { // Test 1: A negative sequence is treated the same way rather than matching nothing.
		Name: "negative", Seq: -1, WantIDs: []string{"anc_5", "anc_10", "anc_20"},
	}, { // Test 2: Exactly at an anchor's sequence includes that anchor.
		Name: "at an anchor", Seq: 10, WantIDs: []string{"anc_5", "anc_10"},
	}, { // Test 3: One below an anchor excludes it.
		Name: "just below", Seq: 9, WantIDs: []string{"anc_5"},
	}, { // Test 4: Below every anchor returns none rather than all.
		Name: "below all", Seq: 1, WantIDs: nil,
	}, { // Test 5: Above every anchor returns all of them.
		Name: "above all", Seq: 1000, WantIDs: []string{"anc_5", "anc_10", "anc_20"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := anchors.Anchors(ctx, test.Seq)
			if err != nil {
				t.Fatalf("Anchors(%d) error = %v", test.Seq, err)
			}
			ids := make([]string, len(got))
			for i, a := range got {
				ids[i] = a.ID
			}
			want := test.WantIDs
			if want == nil {
				want = []string{}
			}
			if diff := cmp.Diff(want, ids); diff != "" {
				t.Errorf("Anchors(%s) (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestAnAnchorCannotBeQuietlyReplaced pins that saving an anchor id twice is refused. An anchor
// records a value a third party stamped, so a second save under the same id would overwrite a
// proof somebody else issued with one this install chose, and a store that accepted it would let
// an anchored coordinate be moved without anybody seeing it happen.
func TestAnAnchorCannotBeQuietlyReplaced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, anchors, _ := auditStores(t, openStore(t))

	original := &audit.Anchor{
		ID: "anc_1", Type: audit.AnchorRFC3161, Shape: audit.AnchorShapeLinear, Seq: 5,
		Link: "genuine", At: baseTime, Ref: "https://freetsa.org/tsr", Proof: "MIIGenuine",
	}
	if err := anchors.SaveAnchor(ctx, original); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	replacement := &audit.Anchor{
		ID: "anc_1", Type: audit.AnchorRFC3161, Shape: audit.AnchorShapeLinear, Seq: 5,
		Link: "rewritten", At: baseTime, Ref: "https://freetsa.org/tsr", Proof: "MIIForged",
	}
	if err := anchors.SaveAnchor(ctx, replacement); err == nil {
		t.Error("an anchor was saved twice under one id, so a stamped proof can be replaced " +
			"without a trace")
	}
	got, err := anchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}
	if len(got) != 1 || got[0].Link != "genuine" || got[0].Proof != "MIIGenuine" {
		t.Errorf("stored anchors = %+v, want the original untouched", got)
	}

	// Deleting one that is not there is reported rather than silently succeeding.
	if err := anchors.DeleteAnchor(ctx, "anc_ghost"); !errors.Is(err, audit.ErrAnchorNotFound) {
		t.Errorf("DeleteAnchor(missing) = %v, want ErrAnchorNotFound", err)
	}
	if err := anchors.DeleteAnchor(ctx, "anc_1"); err != nil {
		t.Errorf("DeleteAnchor() error = %v", err)
	}
}

// TestBindInstallStampsOnlyLaterAppends pins the binding that stops a receipt from being presented
// as another install's history. The install id is folded into a link, so it has to be stamped
// before the entry is hashed and it must never be applied retroactively: an entry already written
// commits to what it committed to, and rewriting the field would break every link after it.
func TestBindInstallStampsOnlyLaterAppends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, binder := auditStores(t, openStore(t))

	if err := store.Append(ctx, mutation(baseTime, "sam", "POST", "/v1/runs")); err != nil {
		t.Fatalf("Append(before binding) error = %v", err)
	}
	binder.BindInstall("in_abc123")
	if err := store.Append(ctx, mutation(baseTime.Add(time.Second), "sam", "POST",
		"/v1/runs")); err != nil {
		t.Fatalf("Append(after binding) error = %v", err)
	}
	// An entry that already names an install keeps its own, so relayed history is not restamped.
	carried := mutation(baseTime.Add(2*time.Second), "sam", "POST", "/v1/runs")
	carried.InstallID = "in_elsewhere"
	if err := store.Append(ctx, carried); err != nil {
		t.Fatalf("Append(with its own install) error = %v", err)
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("Chain() len = %d, want 3", len(chain))
	}
	if chain[0].InstallID != "" {
		t.Errorf("the entry written before the binding reads install %q, want it left unstamped: "+
			"a link already written commits to what it committed to", chain[0].InstallID)
	}
	if chain[1].InstallID != "in_abc123" {
		t.Errorf("the entry written after the binding reads install %q, want in_abc123",
			chain[1].InstallID)
	}
	if chain[2].InstallID != "in_elsewhere" {
		t.Errorf("an entry naming its own install was restamped to %q", chain[2].InstallID)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at position %d across the binding change", at)
	}
	// The caller's own entry is updated in place with what was persisted, so a receipt handed back
	// to a client names the same link the chain holds.
	if carried.Seq != 3 || carried.Hash == "" || carried.PrevHash != chain[1].Hash {
		t.Errorf("the appended entry was not filled in with its chain fields: %+v", carried)
	}
}

// TestTheUniqueSequenceIndexRefusesAFork pins the cross-process backstop under the append mutex.
// The mutex only serializes appends inside one process, and the documented deployment has a worker
// opening the same file. Two processes computing the same next sequence must not both land: one
// has to be refused by the database, or the chain forks and every verification after it fails with
// no way to tell which branch is the real history.
func TestTheUniqueSequenceIndexRefusesAFork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fork.db")
	db := openStoreAt(t, path)
	store, _, _ := auditStores(t, db)

	if err := store.Append(ctx, mutation(baseTime, "sam", "POST", "/v1/runs")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 || chain[0].Seq != 1 {
		t.Fatalf("Chain() = %+v, want one entry at sequence 1", chain)
	}

	// A second writer computing the same next sequence, which is what two processes racing produce.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	const insert = `INSERT INTO audit_entries
	(id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, prev_hash, hash, nonce, install_id)
VALUES (?, ?, 'mallory', '', '', 'POST', '/v1/runs', '', ?, '', 'forged', '', '')`
	if _, err := raw.Exec(insert, "aud_fork_a", "2026-07-01T13:00:00Z", 2); err != nil {
		t.Fatalf("the first writer at sequence 2 was refused: %v", err)
	}
	if _, err := raw.Exec(insert, "aud_fork_b", "2026-07-01T13:00:01Z", 2); err == nil {
		t.Error("two entries were written at the same chain sequence, so the trail forked and no " +
			"verifier can say which branch is the history")
	}
}

// TestConcurrentAppendsAndBeatsProduceOneUnbrokenChain pins the whole append path under load. The
// chain's value is that it is linear and verifiable, and both properties are decided by the
// interaction of the append mutex, the head read inside the transaction, and the unique sequence
// index. Mixing ordinary appends with beat minting is the case where those three have to agree,
// because a beat reads the head, the newest beat, and the count in one step.
func TestConcurrentAppendsAndBeatsProduceOneUnbrokenChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := auditStores(t, openStore(t))

	const (
		writers = 6
		each    = 15
	)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		beats int
		clock = baseTime
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if w%3 == 0 {
					// A beat needs a strictly advancing time, so the emitters share one clock.
					mu.Lock()
					clock = clock.Add(time.Second)
					at := clock
					mu.Unlock()
					if _, err := store.AppendSpanBeat(ctx, at, 60); err != nil {
						if errors.Is(err, audit.ErrClockBehind) {
							continue
						}
						t.Errorf("AppendSpanBeat() error = %v", err)
						return
					}
					mu.Lock()
					beats++
					mu.Unlock()
					continue
				}
				if err := store.Append(ctx, mutation(baseTime, fmt.Sprintf("actor-%d", w),
					"POST", fmt.Sprintf("/v1/runs/%d-%d", w, i))); err != nil {
					t.Errorf("Append() error = %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Fatalf("Verify() reported a break at position %d of %d after concurrent appends", at,
			len(chain))
	}
	seen := make(map[int64]bool, len(chain))
	for i, e := range chain {
		if seen[e.Seq] {
			t.Fatalf("sequence %d appears twice, so the chain forked", e.Seq)
		}
		seen[e.Seq] = true
		if e.Seq != int64(i+1) {
			t.Fatalf("entry %d carries sequence %d, want a contiguous chain", i, e.Seq)
		}
	}

	// Every beat that was accepted is well formed and numbered contiguously, so the feed and any
	// bundle built over the chain agree with the chain itself.
	feed, err := store.SpanBeats(ctx, beats+10)
	if err != nil {
		t.Fatalf("SpanBeats() error = %v", err)
	}
	if len(feed) != beats {
		t.Errorf("SpanBeats() returned %d beats, want the %d that were accepted", len(feed), beats)
	}
	for i, b := range feed {
		number, _, _, ok := audit.ParseSpanPath(b.Path)
		if !ok {
			t.Fatalf("beat %d has an unreadable path %q", i, b.Path)
		}
		if number != int64(i+1) {
			t.Errorf("beat at position %d is numbered %d, want %d", i, number, i+1)
		}
	}
}

// TestChainScanStreamsTheSameChainAsChain pins that the streaming reader and the materializing one
// answer identically. Verification of a long trail goes through the scan so the whole chain is
// never held in memory, and a scan that skipped or reordered an entry would report a break in a
// chain that is intact, or worse, miss one that is not.
func TestChainScanStreamsTheSameChainAsChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := auditStores(t, openStore(t))

	const total = 12
	for i := 0; i < total; i++ {
		if err := store.Append(ctx, mutation(baseTime.Add(time.Duration(i)*time.Second),
			"sam", "POST", fmt.Sprintf("/v1/runs/%d", i))); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}

	tests := []struct {
		Name      string
		AfterSeq  int64
		WantCount int
	}{{ // Test 0: From the start streams the whole chain.
		Name: "from genesis", AfterSeq: 0, WantCount: total,
	}, { // Test 1: A negative cursor is before genesis, so it streams everything too.
		Name: "negative", AfterSeq: -5, WantCount: total,
	}, { // Test 2: A cursor mid-chain streams strictly past it.
		Name: "mid chain", AfterSeq: 4, WantCount: total - 4,
	}, { // Test 3: A cursor at the head streams nothing rather than repeating the head.
		Name: "at head", AfterSeq: total, WantCount: 0,
	}, { // Test 4: A cursor past the head streams nothing rather than failing.
		Name: "past head", AfterSeq: total + 100, WantCount: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got []*audit.Entry
			if err := store.ChainScan(ctx, test.AfterSeq, func(e *audit.Entry) error {
				cp := *e
				got = append(got, &cp)
				return nil
			}); err != nil {
				t.Fatalf("ChainScan(%s) error = %v", test.Name, err)
			}
			if len(got) != test.WantCount {
				t.Fatalf("ChainScan(%s) streamed %d entries, want %d", test.Name, len(got),
					test.WantCount)
			}
			for i, e := range got {
				want := chain[len(chain)-test.WantCount+i]
				if e.Seq != want.Seq || e.Hash != want.Hash || e.Path != want.Path ||
					!e.At.Equal(want.At) {
					t.Errorf("%s: streamed entry %d = %+v, want %+v", test.Name, i, e, want)
				}
			}
		})
	}

	// A scanner that stops is obeyed at once, and its error comes back unwrapped so the caller can
	// match its own sentinel.
	stop := errors.New("stop here")
	calls := 0
	err = store.ChainScan(ctx, 0, func(*audit.Entry) error {
		calls++
		if calls == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Errorf("ChainScan() error = %v, want the scanner's own error back", err)
	}
	if calls != 3 {
		t.Errorf("the scanner was called %d times after asking to stop at 3", calls)
	}
}

// TestAuditEntriesCarryHostileTextIntoAVerifiableLink pins that a path or actor holding bytes a
// text column cannot represent still produces a chain a third party can recompute. The recorded
// path is whatever a caller requested, so it carries whatever they sent, and a link that no
// independent verifier can reproduce is a link that proves nothing.
func TestAuditEntriesCarryHostileTextIntoAVerifiableLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := auditStores(t, openStore(t))

	entries := []*audit.Entry{
		mutation(baseTime, "sam", "POST", "/v1/runs?q=ünïcode%20🔧"),
		mutation(baseTime.Add(time.Second), "actor\x00with-nul", "POST", "/v1/runs"),
		mutation(baseTime.Add(2*time.Second), "actor\xff\xfe", "DELETE", "/v1/runs/\xff"),
		mutation(baseTime.Add(3*time.Second), "", "GET", "/"),
	}
	for i, e := range entries {
		if err := store.Append(ctx, e); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != len(entries) {
		t.Fatalf("Chain() len = %d, want %d", len(chain), len(entries))
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at position %d: a link over hostile text has to be "+
			"reproducible or the chain proves nothing", at)
	}
	// What the store returns has to equal what the caller was handed, or a receipt names a link
	// the chain does not hold.
	for i, e := range entries {
		if chain[i].Hash != e.Hash || chain[i].Actor != e.Actor || chain[i].Path != e.Path {
			t.Errorf("entry %d read back as %+v, want the appended %+v", i, chain[i], e)
		}
	}
}
