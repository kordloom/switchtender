package witness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/identity"
)

// link returns a distinct sixty-four character chain link for label, the shape a real head has, so
// a test feed is refused for the reason under test rather than for carrying an implausible head.
func link(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

// chainBeats returns a clean run of n beats numbered 1..n at chain positions 1..n, each carrying
// its own link. It is the honest stream every tampering test starts from.
func chainBeats(n int64) []Beat {
	out := make([]Beat, 0, n)
	for i := int64(1); i <= n; i++ {
		out = append(out, beat(i, i, link(fmt.Sprintf("beat-%d", i))))
	}
	return out
}

// copyBeats returns an independent copy, so a table case altering one position cannot leak its
// alteration into the next case.
func copyBeats(in []Beat) []Beat {
	out := make([]Beat, len(in))
	copy(out, in)
	return out
}

// kindsOf lists the finding kinds a check produced, in order, so a table can compare them.
func kindsOf(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Kind)
	}
	return out
}

// hasKind reports whether any finding carries kind.
func hasKind(findings []Finding, kind string) bool {
	for _, f := range findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// chainLen is how long the tampering tests make their chain. It is long enough that the oldest,
// the newest, and several interior positions are all distinct cases.
const chainLen = 12

// TestEveryPositionInTheChainIsTamperEvident walks the chain and rewrites the head at each
// position in turn, one position per case. This is the property the whole product rests on: a
// witness that catches a rewrite at the newest position but not at the oldest still-served one
// hands an operator a safe place to edit history. The oldest position is the interesting one,
// because it is the first the memory would forget if the cap were ever set below the feed limit.
func TestEveryPositionInTheChainIsTamperEvident(t *testing.T) {
	t.Parallel()
	honest := chainBeats(chainLen)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := make([]struct {
		// Position is the index in the served run whose head the operator rewrites.
		Position int
	}, 0, chainLen)
	for i := 0; i < chainLen; i++ {
		tests = append(tests, struct{ Position int }{Position: i})
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// Test N: The head at one chain position is rewritten and everything else stands.
			first, findings, err := Check(nil, "https://st.example", honest, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if len(findings) != 0 {
				t.Fatalf("baseline findings = %v, want a clean baseline", kindsOf(findings))
			}
			tampered := copyBeats(honest)
			tampered[test.Position].Head = link("forged")
			next, findings, err := Check(first, "https://st.example", tampered, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if !hasKind(findings, "rewritten_beat") && !hasKind(findings, "rewritten_history") {
				t.Fatalf("rewriting position %d raised %v, want the rewrite named",
					test.Position, kindsOf(findings))
			}
			// The witness must not sign the forged head into its own testimony, whichever position
			// carried it: every beat after a rewrite is built on the replaced history.
			if next.LastHead != honest[len(honest)-1].Head {
				t.Errorf("checkpoint head = %q, want the head first witnessed, not the rewrite",
					next.LastHead)
			}
			if got := next.Recent[tampered[test.Position].Beat].Head; got != honest[test.Position].Head {
				t.Errorf("memory of beat %d = %q, want the head first witnessed",
					tampered[test.Position].Beat, got)
			}
		})
	}
}

// TestEveryChainPositionRelabelIsCaught rewrites the chain position a beat claims rather than its
// head. A history rewritten by deleting an entry leaves every later beat carrying the same content
// at a lower position, so a witness that compared only heads would read the whole shift as clean.
func TestEveryChainPositionRelabelIsCaught(t *testing.T) {
	t.Parallel()
	honest := chainBeats(chainLen)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for testNum := 0; testNum < chainLen; testNum++ {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// Test N: One beat keeps its head and its number but claims a different chain position.
			first, _, err := Check(nil, "https://st.example", honest, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			tampered := copyBeats(honest)
			tampered[testNum].Seq = honest[testNum].Seq + 1000
			_, findings, err := Check(first, "https://st.example", tampered, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if !hasKind(findings, "rewritten_beat") {
				t.Errorf("relabeling position %d raised %v, want rewritten_beat",
					testNum, kindsOf(findings))
			}
		})
	}
}

// TestRemovingEachBeatFromTheFeed pins what disappearance means at each position. Dropping the
// oldest beat is a rolling window sliding and must stay silent, or every healthy witness alerts
// once per beat forever. Dropping an interior beat is an entry that is gone. Dropping the newest is
// the chain losing its tail, the case a chain cannot prove about itself.
func TestRemovingEachBeatFromTheFeed(t *testing.T) {
	t.Parallel()
	honest := chainBeats(chainLen)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := make([]struct {
		// Drop is the index removed from the served run.
		Drop int
		// WantKind is the finding kind the removal must raise, empty when it must raise nothing.
		WantKind string
	}, 0, chainLen)
	for i := 0; i < chainLen; i++ {
		want := "missing_beat"
		switch i {
		case 0:
			want = ""
		case chainLen - 1:
			want = "head_regression"
		}
		tests = append(tests, struct {
			Drop     int
			WantKind string
		}{Drop: i, WantKind: want})
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// Test N: One beat is removed from the answer at a given depth.
			first, _, err := Check(nil, "https://st.example", honest, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			served := append(copyBeats(honest[:test.Drop]), honest[test.Drop+1:]...)
			_, findings, err := Check(first, "https://st.example", served, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if test.WantKind == "" {
				if len(findings) != 0 {
					t.Errorf("the window sliding past beat %d raised %v, want nothing: a healthy "+
						"feed drops its oldest beat every time it advances",
						honest[test.Drop].Beat, kindsOf(findings))
				}
				return
			}
			if !hasKind(findings, test.WantKind) {
				t.Errorf("removing beat %d raised %v, want %s",
					honest[test.Drop].Beat, kindsOf(findings), test.WantKind)
			}
		})
	}
}

// TestTruncationAtEveryDepthIsCaught cuts the tail off the chain at every depth, down to an empty
// feed. Truncation is the one attack a hash chain cannot report on itself, so the witness reporting
// it at every depth, including a feed that goes completely silent, is why the witness exists.
func TestTruncationAtEveryDepthIsCaught(t *testing.T) {
	t.Parallel()
	honest := chainBeats(chainLen)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for testNum := 0; testNum < chainLen; testNum++ {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// Test N: The feed serves only the first N beats of a chain of twelve.
			first, _, err := Check(nil, "https://st.example", honest, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			next, findings, err := Check(first, "https://st.example", honest[:testNum],
				now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if !hasKind(findings, "head_regression") {
				t.Fatalf("a chain cut back to %d beat(s) raised %v, want head_regression",
					testNum, kindsOf(findings))
			}
			// The memory of the tail is what the truncation is measured against, so the truncated
			// answer must not be allowed to take it back.
			if next.LastBeat != int64(chainLen) {
				t.Errorf("checkpoint last beat = %d, want the witnessed %d kept through the "+
					"truncation", next.LastBeat, chainLen)
			}
		})
	}
}

// TestOneTruncationStaysOneEventAtEveryDepth pins the deduplication key on the truncation finding.
// The finding's wording carries the feed's newest beat, which moves every poll while the chain
// regrows, so a key anchored on the moving half would turn one truncation into a fresh event per
// poll and bury the operator.
func TestOneTruncationStaysOneEventAtEveryDepth(t *testing.T) {
	t.Parallel()
	honest := chainBeats(chainLen)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	first, _, err := Check(nil, "https://st.example", honest, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	var d Delta
	cp := first
	recorded := 0
	for depth := 0; depth < chainLen; depth++ {
		var findings []Finding
		cp, findings, err = Check(cp, "https://st.example", honest[:depth],
			now.Add(time.Duration(depth)*time.Minute))
		if err != nil {
			t.Fatalf("Check() error = %v", err)
		}
		recorded += len(d.Fresh(findings))
	}
	if recorded != 1 {
		t.Errorf("one truncation regrowing through %d depths recorded %d events, want 1",
			chainLen, recorded)
	}
}

// TestChainReplacedAtEveryCutPointIsCaught replaces the chain from each position onwards and serves
// the replacement under beat numbers the witness has never seen. Beat numbers are the watched
// server's own counter and it can restart them at will, so a detector keyed on them alone reads a
// wholesale replacement as a healthy stream of new beats. The chain position is the coordinate it
// cannot renumber.
func TestChainReplacedAtEveryCutPointIsCaught(t *testing.T) {
	t.Parallel()
	honest := chainBeats(chainLen)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for testNum := 0; testNum < chainLen; testNum++ {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// Test N: Chain positions from N onwards are replaced and re-served under fresh beat
			// numbers well past anything witnessed.
			first, _, err := Check(nil, "https://st.example", honest, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			replaced := copyBeats(honest)
			for i := testNum; i < chainLen; i++ {
				replaced[i] = beat(int64(1000+i), honest[i].Seq, link(fmt.Sprintf("forged-%d", i)))
			}
			next, findings, err := Check(first, "https://st.example", replaced, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if !hasKind(findings, "rewritten_history") {
				t.Fatalf("replacing the chain from position %d raised %v, want rewritten_history",
					honest[testNum].Seq, kindsOf(findings))
			}
			if next.LastHead != honest[len(honest)-1].Head {
				t.Errorf("checkpoint head = %q, want the last honest head, not the replacement",
					next.LastHead)
			}
		})
	}
}

// TestGapAcrossPollsIsCaughtAtEveryWidth pins that a feed jumping past the witnessed beat between
// polls is reported however wide the jump. Only the witness can see this gap: the beats between the
// two answers never appeared in either one, so nothing inside a single answer contradicts anything.
func TestGapAcrossPollsIsCaughtAtEveryWidth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		// Jump is the beat number the next answer's oldest beat claims.
		Jump int64
		// WantMissing is whether the jump must be reported.
		WantMissing bool
	}{ // Test 0: The next beat in sequence is not a gap.
		{Jump: 6, WantMissing: false},
		// Test 1: One beat skipped is the narrowest real gap.
		{Jump: 7, WantMissing: true},
		// Test 2: A wide jump.
		{Jump: 5000, WantMissing: true},
		// Test 3: A jump to the largest beat number the witness will remember.
		{Jump: maxBeatJump, WantMissing: true},
		// Test 4: Re-serving the witnessed beat itself is a window that has not moved, not a gap.
		{Jump: 5, WantMissing: false},
	}
	honest := chainBeats(5)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			first, _, err := Check(nil, "https://st.example", honest, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			served := []Beat{beat(test.Jump, 100000, link("jumped"))}
			_, findings, err := Check(first, "https://st.example", served, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if got := hasKind(findings, "missing_beat"); got != test.WantMissing {
				t.Errorf("a jump to beat %d raised %v, want missing_beat = %v",
					test.Jump, kindsOf(findings), test.WantMissing)
			}
		})
	}
}

// TestPlausibleBeatsRefusesWhatItCannotCheck pins the boundary of what enters signed memory. A beat
// the witness cannot check is a value the watched server chose, and adopting one wedges the
// checkpoint at whatever it claims while raising nothing, so the refusal has to be exact at the
// edges rather than roughly right.
func TestPlausibleBeatsRefusesWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	good := link("ok")
	tests := []struct {
		// In is the single beat offered to the witness.
		In Beat
		// WantKept is whether the beat may enter signed memory.
		WantKept bool
	}{ // Test 0: An ordinary beat is kept.
		{In: beat(1, 1, good), WantKept: true},
		// Test 1: Beat zero is not a beat; the feed numbers from one.
		{In: beat(0, 1, good), WantKept: false},
		// Test 2: A negative beat number.
		{In: beat(-1, 1, good), WantKept: false},
		// Test 3: Chain position zero.
		{In: beat(1, 0, good), WantKept: false},
		// Test 4: A negative chain position.
		{In: beat(1, -1, good), WantKept: false},
		// Test 5: The largest beat number the witness will remember is still remembered.
		{In: beat(maxBeatJump, 1, good), WantKept: true},
		// Test 6: One past it is a number chosen to wedge the checkpoint.
		{In: beat(maxBeatJump+1, 1, good), WantKept: false},
		// Test 7: An empty head cannot be compared against anything.
		{In: beat(1, 1, ""), WantKept: false},
		// Test 8: A one character head is not a hash but it is comparable, which is what catches a
		// rewrite, so it is kept rather than policed.
		{In: beat(1, 1, "a"), WantKept: true},
		// Test 9: A head at the length bound.
		{In: beat(1, 1, strings.Repeat("a", maxHeadLen)), WantKept: true},
		// Test 10: One byte past the bound is a payload, not a hash.
		{In: beat(1, 1, strings.Repeat("a", maxHeadLen+1)), WantKept: false},
		// Test 11: A multibyte head at the byte bound, since the bound counts bytes.
		{In: beat(1, 1, strings.Repeat("é", maxHeadLen/2)), WantKept: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			kept, findings := plausibleBeats([]Beat{test.In})
			var want []Beat
			if test.WantKept {
				want = []Beat{test.In}
			}
			if diff := cmp.Diff(want, kept, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("plausibleBeats() kept mismatch (-want +got):\n%s", diff)
			}
			if test.WantKept && len(findings) != 0 {
				t.Errorf("a plausible beat raised %v, want nothing", kindsOf(findings))
			}
			if !test.WantKept && !hasKind(findings, "malformed_feed") {
				t.Errorf("a refused beat raised %v, want malformed_feed, so the refusal is on record",
					kindsOf(findings))
			}
		})
	}
}

// TestManyRefusedBeatsStayOneFinding pins that a feed of nothing but garbage produces one finding
// naming the count, not one per beat. A witness that turns a thousand malformed beats into a
// thousand disk records and a thousand notifications is a witness an operator mutes.
func TestManyRefusedBeatsStayOneFinding(t *testing.T) {
	t.Parallel()
	beats := make([]Beat, 0, FeedLimit)
	for i := 0; i < FeedLimit; i++ {
		beats = append(beats, beat(0, 0, ""))
	}
	kept, findings := plausibleBeats(beats)
	if len(kept) != 0 {
		t.Errorf("kept %d implausible beats, want none in signed memory", len(kept))
	}
	if diff := cmp.Diff([]string{"malformed_feed"}, kindsOf(findings)); diff != "" {
		t.Errorf("findings mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(findings[0].Detail, fmt.Sprintf("%d beat(s)", FeedLimit)) {
		t.Errorf("detail = %q, want the refused count named", findings[0].Detail)
	}
}

// TestPlausibleHeadBoundaries pins the head bound on its own, since it is the cheap check standing
// between an untrusted feed and both the signed memory and the findings record.
func TestPlausibleHeadBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the head the feed served.
		In string
		// WantResult is whether it could be a chain link.
		WantResult bool
	}{ // Test 0: Empty is not a head.
		{In: "", WantResult: false},
		// Test 1: One character is comparable, which is all the witness needs.
		{In: "a", WantResult: true},
		// Test 2: A real chain link.
		{In: link("x"), WantResult: true},
		// Test 3: The length bound exactly.
		{In: strings.Repeat("a", maxHeadLen), WantResult: true},
		// Test 4: One byte past the bound.
		{In: strings.Repeat("a", maxHeadLen+1), WantResult: false},
		// Test 5: Non-hex is still comparable, so the charset is deliberately not policed.
		{In: "not a hash at all", WantResult: true},
		// Test 6: A head that is nothing but a newline would break a line-oriented record, and it is
		// still remembered, so the record's own bounds are what must hold.
		{In: "\n", WantResult: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := plausibleHead(test.In); got != test.WantResult {
				t.Errorf("plausibleHead(%d bytes) = %v, want %v", len(test.In), got, test.WantResult)
			}
		})
	}
}

// TestCheckDoesNotMutateThePreviousCheckpoint pins that a check is pure with respect to the memory
// handed to it. The hosted service keeps one checkpoint in memory and hands the same pointer to
// every poll, so a check that wrote through into its argument would let a later poll quietly edit
// the memory an attestation already reported.
func TestCheckDoesNotMutateThePreviousCheckpoint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	first, _, err := Check(nil, "https://st.example", chainBeats(3), now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	before := *first
	beforeRecent := map[int64]Observed{}
	for k, v := range first.Recent {
		beforeRecent[k] = v
	}
	beforePositions := map[int64]string{}
	for k, v := range first.Positions {
		beforePositions[k] = v
	}
	if _, _, err := Check(first, "https://st.example", chainBeats(9), now.Add(time.Hour)); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if before.LastBeat != first.LastBeat || before.LastHead != first.LastHead ||
		before.LastSeq != first.LastSeq || !before.ObservedAt.Equal(first.ObservedAt) {
		t.Errorf("the previous checkpoint was edited: %+v became %+v", before, *first)
	}
	if diff := cmp.Diff(beforeRecent, first.Recent, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the previous checkpoint's beat memory was edited (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(beforePositions, first.Positions, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the previous checkpoint's position memory was edited (-before +after):\n%s", diff)
	}
}

// TestMemoryForgetsTheOldestNotTheNewest pins which end of the memory the cap trims. Forgetting the
// newest would hand a truncating operator the exact window they need: a beat the feed still serves
// but the witness no longer remembers is re-adopted as if new, so its rewrite raises nothing.
func TestMemoryForgetsTheOldestNotTheNewest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	first, findings, err := Check(nil, "https://st.example", chainBeats(FeedLimit), now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("baseline findings = %v, want none", kindsOf(findings))
	}
	// The feed advances by a full window: every beat it serves is new.
	next := make([]Beat, 0, FeedLimit)
	for i := int64(FeedLimit + 1); i <= 2*FeedLimit; i++ {
		next = append(next, beat(i, i, link(fmt.Sprintf("beat-%d", i))))
	}
	after, findings, err := Check(first, "https://st.example", next, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("an advancing window raised %v, want nothing", kindsOf(findings))
	}
	if len(after.Recent) != recentCap {
		t.Errorf("memory holds %d beats, want the %d cap", len(after.Recent), recentCap)
	}
	if len(after.Positions) != recentCap {
		t.Errorf("position memory holds %d entries, want the %d cap",
			len(after.Positions), recentCap)
	}
	if _, ok := after.Recent[2*FeedLimit]; !ok {
		t.Error("the newest beat was forgotten, so its rewrite would be re-adopted in silence")
	}
	if _, ok := after.Recent[1]; ok {
		t.Error("the oldest beat survived the cap, so the memory is not bounded")
	}
	if _, ok := after.Positions[2*FeedLimit]; !ok {
		t.Error("the newest chain position was forgotten")
	}
}

// TestMemoryStillCoversEverythingTheFeedServes pins the invariant behind the cap: what the witness
// remembers is at least what the feed serves. A witness remembering less than is served has a hole
// at the oldest end of every answer that an operator can rewrite for free.
func TestMemoryStillCoversEverythingTheFeedServes(t *testing.T) {
	t.Parallel()
	if recentCap < FeedLimit {
		t.Fatalf("recentCap %d is below FeedLimit %d, so every answer's oldest beats are forgotten "+
			"and their rewrites are re-adopted as first sightings", recentCap, FeedLimit)
	}
}

// TestStalledFeedFiresOnlyPastTheBound pins the staleness boundary in both directions. Firing at or
// below the bound reports a quiet chain as frozen and trains the reader to ignore the channel;
// never firing lets a stopped beat writer or a replayed answer be attested healthy forever.
func TestStalledFeedFiresOnlyPastTheBound(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	frozen := chainBeats(3)
	tests := []struct {
		// Elapsed is how long the newest head has stood unchanged.
		Elapsed time.Duration
		// WantStalled is whether the feed must be reported as stopped.
		WantStalled bool
	}{ // Test 0: A few minutes of quiet is a chain with nothing to say.
		{Elapsed: 5 * time.Minute, WantStalled: false},
		// Test 1: One nanosecond under the bound still is not a finding.
		{Elapsed: StaleAfter - time.Nanosecond, WantStalled: false},
		// Test 2: Exactly the bound is not past it.
		{Elapsed: StaleAfter, WantStalled: false},
		// Test 3: One nanosecond past the bound is.
		{Elapsed: StaleAfter + time.Nanosecond, WantStalled: true},
		// Test 4: A week of no movement.
		{Elapsed: 7 * 24 * time.Hour, WantStalled: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			first, _, err := Check(nil, "https://st.example", frozen, start)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			_, findings, err := Check(first, "https://st.example", frozen, start.Add(test.Elapsed))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if got := hasKind(findings, "stalled_feed"); got != test.WantStalled {
				t.Errorf("after %v unmoved the findings were %v, want stalled_feed = %v",
					test.Elapsed, kindsOf(findings), test.WantStalled)
			}
		})
	}
}

// TestObservationTimeBelongsToTheHeadNotThePoll pins that the timestamp the staleness check
// measures against moves only when the head moves. Re-stamping it on every poll made the comparison
// measure the poll interval, which is always well under the bound, so a frozen chain could never be
// reported however long it stood.
func TestObservationTimeBelongsToTheHeadNotThePoll(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	frozen := chainBeats(3)
	cp, _, err := Check(nil, "https://st.example", frozen, start)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	for i := 1; i <= 10; i++ {
		cp, _, err = Check(cp, "https://st.example", frozen, start.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("Check() error = %v", err)
		}
		if !cp.ObservedAt.Equal(start) {
			t.Fatalf("poll %d re-stamped the observation time to %s, want it anchored to when the "+
				"head last moved (%s)", i, cp.ObservedAt, start)
		}
	}
	// A head that moves re-stamps, or the staleness bound would be measured from the beginning of
	// time and a healthy feed would be reported frozen.
	moved := append(copyBeats(frozen), beat(4, 4, link("beat-4")))
	after, _, err := Check(cp, "https://st.example", moved, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !after.ObservedAt.Equal(start.Add(time.Hour)) {
		t.Errorf("observation time = %s, want the poll that saw the head move", after.ObservedAt)
	}
}

// TestEmptyFeedIsNeverSilent pins that a witness with nothing to witness says so rather than
// signing a checkpoint that reads as health. A nightly cron over a server whose span beat was never
// turned on reported success every night while covering nothing at all.
func TestEmptyFeedIsNeverSilent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		// Beats is what the feed answers.
		Beats []Beat
		// WantKind is the finding an empty answer must raise.
		WantKind string
	}{ // Test 0: A nil answer with no memory behind it.
		{Beats: nil, WantKind: "empty_feed"},
		// Test 1: An empty but present array is the same thing.
		{Beats: []Beat{}, WantKind: "empty_feed"},
		// Test 2: An answer of nothing but garbage empties to the same place, and the refusal is
		// reported too, so a hostile feed cannot buy silence by serving nonsense.
		{Beats: []Beat{beat(0, 0, "")}, WantKind: "empty_feed"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			next, findings, err := Check(nil, "https://st.example", test.Beats, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if !hasKind(findings, test.WantKind) {
				t.Errorf("findings = %v, want %s", kindsOf(findings), test.WantKind)
			}
			if next == nil {
				t.Fatal("Check() returned no checkpoint")
			}
			if next.LastBeat != 0 {
				t.Errorf("checkpoint adopted beat %d from an empty answer", next.LastBeat)
			}
		})
	}
}

// TestCheckRefusesAnotherServersMemory pins the refusal that keeps one witness from inventing
// findings out of two unrelated chains. Reporting the mismatch instead of refusing would also
// overwrite the memory that would have caught a real rewrite of the server it does watch.
func TestCheckRefusesAnotherServersMemory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		// Remembered is the server the checkpoint was written for.
		Remembered string
		// Watched is the server whose feed is being held against it.
		Watched string
		// WantRefused is whether the pairing must be refused outright.
		WantRefused bool
	}{ // Test 0: The same server, spelled identically.
		{Remembered: "https://a.example", Watched: "https://a.example", WantRefused: false},
		// Test 1: A trailing slash is the same server; refusing would blind the witness forever
		// while it reported itself healthy.
		{Remembered: "https://a.example", Watched: "https://a.example/", WantRefused: false},
		// Test 2: Scheme and host are case-insensitive on the wire.
		{Remembered: "https://a.example", Watched: "HTTPS://A.EXAMPLE", WantRefused: false},
		// Test 3: A different host is a different chain.
		{Remembered: "https://a.example", Watched: "https://b.example", WantRefused: true},
		// Test 4: The same host over a different scheme is a different endpoint.
		{Remembered: "https://a.example", Watched: "http://a.example", WantRefused: true},
		// Test 5: An explicit port is not the same spelling as none, and the witness would fetch a
		// different URL, so the memories must not be merged.
		{Remembered: "https://a.example", Watched: "https://a.example:443", WantRefused: true},
		// Test 6: A path below the host is a different base URL.
		{Remembered: "https://a.example", Watched: "https://a.example/base", WantRefused: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			prev, _, err := Check(nil, test.Remembered, chainBeats(2), now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			_, _, err = Check(prev, test.Watched, chainBeats(3), now.Add(time.Minute))
			if got := err != nil; got != test.WantRefused {
				t.Errorf("holding %s memory against %s gave err = %v, want refused = %v",
					test.Remembered, test.Watched, err, test.WantRefused)
			}
		})
	}
}

// TestNormalizeServerTable pins the one spelling the checkpoint, the pin, and the feed request all
// agree on. Two spellings that normalize apart become two memories, each blind to what the other
// witnessed, and two that wrongly normalize together merge two chains into invented findings.
func TestNormalizeServerTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the base URL as an operator spelled it.
		In string
		// WantResult is the normalized form.
		WantResult string
	}{ // Test 0: An already normal URL is untouched.
		{In: "https://st.example", WantResult: "https://st.example"},
		// Test 1: A trailing slash is trimmed.
		{In: "https://st.example/", WantResult: "https://st.example"},
		// Test 2: Several trailing slashes are all trimmed.
		{In: "https://st.example///", WantResult: "https://st.example"},
		// Test 3: Scheme and host are lowercased because they are case-insensitive on the wire.
		{In: "HTTPS://ST.EXAMPLE", WantResult: "https://st.example"},
		// Test 4: A path below the host keeps its case, because paths are case-sensitive.
		{In: "HTTPS://ST.EXAMPLE/Base", WantResult: "https://st.example/Base"},
		// Test 5: A port survives.
		{In: "https://ST.example:8443", WantResult: "https://st.example:8443"},
		// Test 6: An IPv6 literal survives its brackets.
		{In: "http://[::1]:8080", WantResult: "http://[::1]:8080"},
		// Test 7: Empty stays empty rather than becoming a URL.
		{In: "", WantResult: ""},
		// Test 8: A bare host with no scheme cannot be parsed as a base URL and is left as given,
		// so the caller sees what they typed rather than a silently invented scheme.
		{In: "st.example", WantResult: "st.example"},
		// Test 9: A value that does not parse at all is returned trimmed, never dropped.
		{In: "ht tp://st.example", WantResult: "ht tp://st.example"},
		// Test 10: A unicode host is left alone rather than half-folded, so it cannot collide with
		// an ASCII spelling.
		{In: "https://ST.EXAMPLE/ü", WantResult: "https://st.example/%C3%BC"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := NormalizeServer(test.In)
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("NormalizeServer(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
			// Normalizing is idempotent, or a checkpoint written from a normalized value would
			// refuse the watch that produced it.
			if again := NormalizeServer(got); again != got {
				t.Errorf("NormalizeServer(%q) = %q, not idempotent", got, again)
			}
			if !sameServer(test.In, got) {
				t.Errorf("sameServer(%q, %q) = false, want the spelling and its normal form to "+
					"address one server", test.In, got)
			}
		})
	}
}

// TestCheckpointRoundTripsThroughDisk pins that everything the witness remembers survives being
// saved and loaded. A field that silently fails to persist is memory the witness loses on every
// restart, and lost memory is exactly the hole a truncation hides in.
func TestCheckpointRoundTripsThroughDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := testIdentity(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	original, _, err := Check(nil, "https://st.example", chainBeats(5), now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	path := filepath.Join(dir, "state.json")
	if err := Save(path, original, id); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := Load(path, id.PublicKeyHex())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if diff := cmp.Diff(original, loaded, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("checkpoint did not round-trip (-saved +loaded):\n%s", diff)
	}
	// The file is written owner-only: it carries the memory a forger wants to replace.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("checkpoint mode = %o, want 600", perm)
	}
	// The loaded memory is usable for the next watch of the same server, or a restart would blind
	// the witness permanently.
	_, findings, err := Check(loaded, "https://st.example", chainBeats(6), now.Add(time.Hour))
	if err != nil {
		t.Errorf("Check() refused the checkpoint it just loaded: %v", err)
	} else if len(findings) != 0 {
		t.Errorf("a reloaded memory invented %v against an honest feed", kindsOf(findings))
	}
}

// TestCheckpointSignatureCoversEveryField flips each field of a signed checkpoint in turn and
// expects verification to fail. The checkpoint is the witness's whole testimony, so any field that
// can be edited without breaking the signature is a field a forger edits for free, and a field
// added later must not quietly land outside the signature.
func TestCheckpointSignatureCoversEveryField(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	id := testIdentity(t)
	c := &Checkpoint{
		Server: "https://st.example", LastBeat: 7, LastSeq: 70, LastHead: link("head"),
		Recent:     map[int64]Observed{7: {Seq: 70, Head: link("head")}},
		Positions:  map[int64]string{70: link("head")},
		ObservedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	}
	if err := Sign(c, id); err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	// Every field but the signature envelope itself must be covered, and the fixture must actually
	// carry them all, or a field would go untested by silently being absent.
	for _, name := range []string{"server", "last_beat", "last_seq", "last_head", "recent",
		"positions", "observed_at"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("the fixture does not carry %s, so that field is not being tested", name)
		}
	}
	for name, value := range fields {
		if name == "public_key" || name == "sig" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tampered := map[string]any{}
			for k, v := range fields {
				tampered[k] = v
			}
			switch v := value.(type) {
			case bool:
				tampered[name] = !v
			case float64:
				tampered[name] = v + 1
			case string:
				if _, err := time.Parse(time.RFC3339, v); err == nil {
					tampered[name] = "2001-01-01T00:00:00Z"
				} else {
					tampered[name] = v + "x"
				}
			case map[string]any:
				altered := map[string]any{}
				for k, mv := range v {
					altered[k] = mv
				}
				altered["999"] = map[string]any{"seq": float64(999), "head": "zz"}
				if name == "positions" {
					altered["999"] = "zz"
				}
				tampered[name] = altered
			default:
				t.Fatalf("field %s has unhandled type %T", name, value)
			}
			doc, err := json.Marshal(tampered)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			var altered Checkpoint
			if err := json.Unmarshal(doc, &altered); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if _, err := Verify(&altered); err == nil {
				t.Errorf("Verify() accepted a change to %s; that field is forgeable", name)
			}
		})
	}
}

// TestVerifyFailsClosedOnAMalformedEnvelope pins that every shape of broken signature material is
// refused with an error rather than accepted or panicked on. The ed25519 verifier panics outright
// on a wrong-sized public key, so the length check standing in front of it is load-bearing: a
// witness that crashes on a crafted state file is a witness that stops watching.
func TestVerifyFailsClosedOnAMalformedEnvelope(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	signed := &Checkpoint{Server: "https://st.example", LastBeat: 1, LastSeq: 1,
		LastHead: link("h"), Recent: map[int64]Observed{}, Positions: map[int64]string{},
		ObservedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
	if err := Sign(signed, id); err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	goodKey, goodSig := signed.PublicKey, signed.Sig
	tests := []struct {
		// PublicKey and Sig are the envelope offered to the verifier.
		PublicKey string
		Sig       string
	}{ // Test 0: Nothing at all, the unsigned document.
		{PublicKey: "", Sig: ""},
		// Test 1: A key with no signature.
		{PublicKey: goodKey, Sig: ""},
		// Test 2: A signature with no key.
		{PublicKey: "", Sig: goodSig},
		// Test 3: A key that is not hex.
		{PublicKey: "zzzz", Sig: goodSig},
		// Test 4: A key of odd hex length.
		{PublicKey: goodKey[:len(goodKey)-1], Sig: goodSig},
		// Test 5: A key that is valid hex but too short; ed25519 panics on this without the guard.
		{PublicKey: "00112233", Sig: goodSig},
		// Test 6: A key that is valid hex but too long.
		{PublicKey: goodKey + "00", Sig: goodSig},
		// Test 7: A signature that is not hex.
		{PublicKey: goodKey, Sig: "zzzz"},
		// Test 8: A signature of the wrong length.
		{PublicKey: goodKey, Sig: "0011"},
		// Test 9: A well-formed signature that is simply wrong.
		{PublicKey: goodKey, Sig: strings.Repeat("00", 64)},
		// Test 10: A well-formed key that did not sign this document.
		{PublicKey: strings.Repeat("11", 32), Sig: goodSig},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			c := *signed
			c.PublicKey, c.Sig = test.PublicKey, test.Sig
			signer, err := Verify(&c)
			if err == nil {
				t.Fatalf("Verify() accepted key %q sig %q", test.PublicKey, test.Sig)
			}
			if signer != "" {
				t.Errorf("Verify() reported signer %q on failure, want none", signer)
			}
			a := &Attestation{Server: "https://st.example", PublicKey: test.PublicKey, Sig: test.Sig}
			if _, err := VerifyAttestation(a); err == nil {
				t.Errorf("VerifyAttestation() accepted key %q sig %q", test.PublicKey, test.Sig)
			}
		})
	}
	// The control: the untouched envelope verifies and names its signer, so the table above is not
	// passing because verification never works.
	signer, err := Verify(signed)
	if err != nil || signer != id.PublicKeyHex() {
		t.Fatalf("Verify() on the honest checkpoint = %q, %v, want this witness's key", signer, err)
	}
}

// TestLoadFailsClosedOnEveryBadStateFile pins that a state file the witness cannot trust is refused
// rather than believed. The state file is the memory a truncation is measured against, so a load
// that silently accepts a replaced or corrupt file hands the watched operator a free reset.
func TestLoadFailsClosedOnEveryBadStateFile(t *testing.T) {
	t.Parallel()
	mine := testIdentity(t)
	forger := testIdentity(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	honest, _, err := Check(nil, "https://st.example", chainBeats(3), now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	signedByForger := func(t *testing.T) []byte {
		t.Helper()
		c := *honest
		if err := Sign(&c, forger); err != nil {
			t.Fatalf("Sign() error = %v", err)
		}
		raw, err := json.Marshal(&c)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		return raw
	}
	tests := []struct {
		// Content is what sits on disk where the checkpoint belongs.
		Content func(t *testing.T) []byte
		// Pin is the key the witness expects, empty for no pin.
		Pin func(t *testing.T) string
		// WantErr is whether the load must fail.
		WantErr bool
	}{{ // Test 0: Empty bytes are not a checkpoint.
		Content: func(*testing.T) []byte { return nil },
		Pin:     func(t *testing.T) string { return mine.PublicKeyHex() }, WantErr: true,
	}, { // Test 1: Not JSON at all.
		Content: func(*testing.T) []byte { return []byte("not json") },
		Pin:     func(t *testing.T) string { return mine.PublicKeyHex() }, WantErr: true,
	}, { // Test 2: Valid JSON of the wrong shape.
		Content: func(*testing.T) []byte { return []byte(`["a","b"]`) },
		Pin:     func(t *testing.T) string { return mine.PublicKeyHex() }, WantErr: true,
	}, { // Test 3: A well-formed but unsigned checkpoint. The signature is the whole point.
		Content: func(*testing.T) []byte { return []byte(`{"server":"https://st.example"}`) },
		Pin:     func(t *testing.T) string { return mine.PublicKeyHex() }, WantErr: true,
	}, { // Test 4: Signed by a forger's own key and internally consistent, which is exactly what a
		// forger with write access produces. The pin is what makes it detectable.
		Content: signedByForger,
		Pin:     func(t *testing.T) string { return mine.PublicKeyHex() }, WantErr: true,
	}, { // Test 5: The same file with no pin is accepted, which is the documented behavior and the
		// reason the caller must always pass its own key.
		Content: signedByForger, Pin: func(*testing.T) string { return "" }, WantErr: false,
	}, { // Test 6: A truncated JSON document.
		Content: func(t *testing.T) []byte { return signedByForger(t)[:20] },
		Pin:     func(*testing.T) string { return "" }, WantErr: true,
	}, { // Test 7: One byte of the signed content edited in place, the shape of an operator who
		// rewrote the memory their truncation would otherwise be measured against.
		Content: func(t *testing.T) []byte {
			raw := signedByForger(t)
			return []byte(strings.Replace(string(raw), `"last_beat":3`, `"last_beat":9`, 1))
		},
		Pin: func(*testing.T) string { return "" }, WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, test.Content(t), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			c, err := Load(path, test.Pin(t))
			if got := err != nil; got != test.WantErr {
				t.Fatalf("Load() error = %v, want an error = %v", err, test.WantErr)
			}
			if test.WantErr && c != nil {
				t.Errorf("Load() returned a checkpoint alongside its error: %+v", c)
			}
		})
	}
}

// TestLoadTreatsAMissingFileAsTheFirstWatch pins the one success that is not a checkpoint. A
// missing file must not be an error, or a witness could never start, and it must not be a
// checkpoint either, or the first watch would compare against invented memory.
func TestLoadTreatsAMissingFileAsTheFirstWatch(t *testing.T) {
	t.Parallel()
	c, err := Load(filepath.Join(t.TempDir(), "absent.json"), testIdentity(t).PublicKeyHex())
	if err != nil || c != nil {
		t.Errorf("Load(absent) = %v, %v, want nil, nil", c, err)
	}
}

// TestLoadReportsAnUnreadableFileRatherThanStartingOver pins that a state file the witness cannot
// read is an error, not a first watch. Treating a permission error as "no memory yet" would let an
// operator blind the witness by making its own memory unreadable and get a clean attestation.
func TestLoadReportsAnUnreadableFileRatherThanStartingOver(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, where file permissions do not refuse a read")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{}"), 0o000); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	c, err := Load(path, "")
	if err == nil {
		t.Fatalf("Load() on an unreadable state file = %+v, want an error, not a fresh start", c)
	}
	if c != nil {
		t.Errorf("Load() returned a checkpoint alongside its error: %+v", c)
	}
}

// TestSaveRefusesAnUnwritableDirectory pins that a checkpoint that cannot be persisted is reported.
// The caller folds the error into a degraded but still alerting poll, and silently dropping it here
// would leave the witness reporting health from memory it never wrote down.
func TestSaveRefusesAnUnwritableDirectory(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, where directory permissions do not refuse a write")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	c, _, err := Check(nil, "https://st.example", chainBeats(2), time.Now())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if err := Save(filepath.Join(dir, "state.json"), c, testIdentity(t)); err == nil {
		t.Error("Save() reported success on a directory it cannot write")
	}
}

// TestSaveReplacesAnExistingCheckpointAtomically pins that a rewrite of the state file leaves
// either the old memory or the new one, never a half-written file. A witness that fails
// verification on its own memory after a crash is a witness that starts over, and starting over is
// the state a truncating operator wants it in.
func TestSaveReplacesAnExistingCheckpointAtomically(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		c, _, err := Check(nil, "https://st.example", chainBeats(i), now)
		if err != nil {
			t.Fatalf("Check() error = %v", err)
		}
		if err := Save(path, c, id); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		got, err := Load(path, id.PublicKeyHex())
		if err != nil {
			t.Fatalf("Load() after overwrite %d error = %v", i, err)
		}
		if got.LastBeat != i {
			t.Errorf("after overwrite %d the memory reads beat %d, want %d", i, got.LastBeat, i)
		}
	}
}

// TestDefaultStatePathIsBesideTheWorkingDirectory pins where a witness with no configuration keeps
// its memory, since an operator who moves between directories and silently gets a fresh memory each
// time is a witness that remembers nothing.
func TestDefaultStatePathIsBesideTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	got := DefaultStatePath()
	if diff := cmp.Diff(filepath.Join(".", "switchtender-witness.json"), got); diff != "" {
		t.Errorf("DefaultStatePath() mismatch (-want +got):\n%s", diff)
	}
}

// TestSignIsDeterministicAndKeyBound pins that the same identity over the same content produces the
// same envelope and that a different identity does not, which is what lets a relying party pin one
// key and reject everything else.
func TestSignIsDeterministicAndKeyBound(t *testing.T) {
	t.Parallel()
	a, b := testIdentity(t), testIdentity(t)
	base := Checkpoint{Server: "https://st.example", LastBeat: 3, LastSeq: 3, LastHead: link("h"),
		Recent: map[int64]Observed{3: {Seq: 3, Head: link("h")}}, Positions: map[int64]string{},
		ObservedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
	one, two, other := base, base, base
	for _, pair := range []struct {
		C  *Checkpoint
		ID identity.Identity
	}{{&one, a}, {&two, a}, {&other, b}} {
		if err := Sign(pair.C, pair.ID); err != nil {
			t.Fatalf("Sign() error = %v", err)
		}
	}
	if one.Sig != two.Sig || one.PublicKey != two.PublicKey {
		t.Error("signing the same checkpoint twice with one key produced two envelopes")
	}
	if other.Sig == one.Sig || other.PublicKey == one.PublicKey {
		t.Error("two different witnesses produced the same envelope")
	}
	// A checkpoint signed by another witness never verifies as this one's, whatever it says.
	if signer, err := Verify(&other); err != nil || signer != b.PublicKeyHex() {
		t.Errorf("Verify() = %q, %v, want the other witness named so a pin can refuse it", signer, err)
	}
}

// TestClipNeverSplitsARune pins the bound that stands between an untrusted feed's head and the
// findings record. Cutting inside a multibyte rune writes invalid UTF-8 into a JSON line, and a
// line the reader cannot parse is a finding that vanishes from every restart's recount.
func TestClipNeverSplitsARune(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the value being shortened and Limit is the byte bound.
		In    string
		Limit int
		// WantResult is the exact shortened value.
		WantResult string
	}{ // Test 0: A value under the bound is untouched.
		{In: "hello", Limit: 10, WantResult: "hello"},
		// Test 1: A value exactly at the bound is untouched, so the ellipsis never lies.
		{In: "hello", Limit: 5, WantResult: "hello"},
		// Test 2: One byte over is cut and marked.
		{In: "hello", Limit: 4, WantResult: "hell..."},
		// Test 3: An empty value at a zero bound is untouched.
		{In: "", Limit: 0, WantResult: ""},
		// Test 4: A cut landing inside a two byte rune backs up to the rune start.
		{In: "aéb", Limit: 2, WantResult: "a..."},
		// Test 5: A cut landing on a rune start keeps the whole rune before it.
		{In: "aéb", Limit: 3, WantResult: "aé..."},
		// Test 6: A cut inside a four byte rune backs all the way out of it.
		{In: "\U0001F600\U0001F600", Limit: 6, WantResult: "\U0001F600..."},
		// Test 7: A value of nothing but one oversized rune loses it entirely rather than emitting
		// half of it.
		{In: "\U0001F600", Limit: 2, WantResult: "..."},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := clip(test.In, test.Limit)
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("clip(%q, %d) mismatch (-want +got):\n%s", test.In, test.Limit, diff)
			}
			if !utf8.ValidString(got) {
				t.Errorf("clip(%q, %d) = %q, which is not valid UTF-8", test.In, test.Limit, got)
			}
		})
	}
}
