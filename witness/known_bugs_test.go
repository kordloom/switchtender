package witness

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests demonstrate defects found while writing the suite. Each one fails against the code as
// it stands, so each is skipped to keep the suite green until the operator decides what to fix.

// TestAHostileFeedCannotBecomeAThousandFindings pins the amplification bound the gap walk already
// keeps and the duplicate walk does not.
//
// The gap arithmetic is summarized on purpose, and the reason is written down beside it: a feed can
// serve a thousand beats with a thousand gaps, and one finding per gap turns each poll into a
// thousand disk records and a thousand notifications. The branch immediately below it, which
// handles a beat that repeats or steps backwards, appends one finding per pair with no summary at
// all. A feed serving the same beat a thousand times therefore produces nine hundred and
// ninety-nine findings from one poll, and because they are byte for byte identical the poll-level
// deduplication does not collapse them either: it only compares against the previous poll's set,
// never within the current one. That is nine hundred and ninety-nine lines appended to the findings
// record, nine hundred and ninety-nine webhook deliveries, and nine hundred and ninety-nine added
// to the findings total an attestation carries, all from one answer the watched server chose to
// send.
func TestAHostileFeedCannotBecomeAThousandFindings(t *testing.T) {
	t.Parallel()
	beats := make([]Beat, 0, FeedLimit)
	for i := 0; i < FeedLimit; i++ {
		beats = append(beats, beat(5, 5, link("repeat")))
	}
	_, findings, err := Check(nil, "https://st.example", beats, fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !hasKind(findings, "duplicate_beat") {
		t.Fatal("a feed serving one beat a thousand times raised nothing")
	}
	if len(findings) > 2 {
		t.Errorf("a feed of %d repeated beats raised %d findings, want them summarized the way "+
			"the gap walk beside them already is", FeedLimit, len(findings))
	}
	// Even if the walk kept emitting one per pair, the poll-level deduplication should collapse
	// byte-identical findings before any of them reaches the record or the notifier.
	var d Delta
	if fresh := d.Fresh(findings); len(fresh) > 2 {
		t.Errorf("%d byte-identical findings survived deduplication, so each becomes its own "+
			"record and its own alert", len(fresh))
	}
}

// TestAStandingGapIsOneEventNotOnePerPoll pins the deduplication key on the in-answer gap finding.
//
// Every other finding whose wording moves with the feed carries a stable Key so it counts as one
// event: the truncation finding anchors on the witnessed beat, the cross-poll gap anchors on the
// witnessed beat, the replaced-position finding anchors on the position, and the stalled finding
// anchors on the beat. The gap walk's finding carries no Key at all, and its wording names the
// oldest and newest beat in the answer. Both of those move every time the feed advances, so a gap
// that simply stands there is a fresh event on every single poll: at a sixty second interval, one
// permanent gap is one thousand four hundred and forty records and one thousand four hundred and
// forty alerts a day, and a findings total that climbs by that much says the witness saw fourteen
// hundred separate events when it saw one.
func TestAStandingGapIsOneEventNotOnePerPoll(t *testing.T) {
	t.Parallel()
	now := fixedClock
	// The chain permanently skips beats 3 to 9, and the feed keeps advancing past the gap.
	served := []Beat{beat(1, 1, link("beat-1")), beat(2, 2, link("beat-2"))}
	for n := int64(10); n <= 14; n++ {
		served = append(served, beat(n, n, link(fmt.Sprintf("beat-%d", n))))
	}
	cp, findings, err := Check(nil, "https://st.example", served, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !hasKind(findings, "missing_beat") {
		t.Fatalf("the standing gap raised %v, want missing_beat", kindsOf(findings))
	}
	var d Delta
	recorded := len(d.Fresh(findings))
	for i := int64(15); i <= 30; i++ {
		served = append(served, beat(i, i, link(fmt.Sprintf("beat-%d", i))))
		cp, findings, err = Check(cp, "https://st.example", served, now.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("Check() error = %v", err)
		}
		recorded += len(d.Fresh(findings))
	}
	if recorded != 1 {
		t.Errorf("one standing gap was recorded %d times across 17 polls, want 1: the finding "+
			"needs a Key anchored on the gap rather than on the ends of the window", recorded)
	}
}

// TestAnImplausibleChainPositionIsRefusedLikeAnImplausibleBeatNumber pins the missing half of the
// plausibility bound.
//
// The beat number is bounded above on purpose, and the reason is written down: a number vastly
// beyond the plausible is a value the watched server chose to wedge the checkpoint with. The chain
// position gets the same lower bound and no upper bound at all, so a server can serve one beat
// claiming chain position 2^62 on the very first watch. Nothing is refused, nothing is reported,
// and the checkpoint adopts it. From that moment every honest answer the server ever gives reports
// a position below the witnessed one, which raises seq_regression forever and refuses to adopt any
// head again. One malformed answer permanently wedges that server's memory, and the only repair is
// an operator deleting the state file, which is the one action that also destroys the memory a
// truncation would have been measured against.
func TestAnImplausibleChainPositionIsRefusedLikeAnImplausibleBeatNumber(t *testing.T) {
	t.Parallel()
	huge := Beat{Beat: 1, Seq: 1 << 62, Head: link("wedge"), At: "2026-08-01T00:00:00Z"}
	kept, findings := plausibleBeats([]Beat{huge})
	if len(kept) != 0 || !hasKind(findings, "malformed_feed") {
		t.Fatalf("plausibleBeats() kept %d beats and raised %v, want the position refused and "+
			"named the way an out-of-range beat number already is", len(kept), kindsOf(findings))
	}
	// And the end-to-end consequence: after such an answer the witness must still be able to
	// witness the server cleanly.
	cp, _, err := Check(nil, "https://st.example", []Beat{huge}, fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	honest := chainBeats(3)
	_, findings, err = Check(cp, "https://st.example", honest, fixedClock.Add(time.Hour))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if hasKind(findings, "seq_regression") {
		t.Error("an honest chain is reported as regressed forever after one out-of-range " +
			"chain position was adopted into signed memory")
	}
}

// TestTwoWatchesOfOneServerDoNotCorruptTheMemory pins that the state file survives two watches
// overlapping.
//
// Save writes to a temporary path derived from the state path alone, so every watcher of one server
// writes the same temporary file, and it writes it with a plain truncating create rather than an
// exclusive one. Two overlapping watches therefore interleave inside one file and rename whatever
// is there. The result is a state file that fails to parse or, worse, one that fails signature
// verification, which is the exact error the witness raises when a state file has been tampered
// with: an operator reading "the document was altered" has no way to tell a race from an attack.
// The failure is durable, because a witness that cannot load its memory never saves a repaired one.
//
// Nothing in the shipped commands runs two watches at once, but the ways to get there are ordinary:
// a cron running the single witness command more often than one fetch takes, two crons on one state
// path, or any caller of the exported CheckAll while the service's own ticker is running.
func TestTwoWatchesOfOneServerDoNotCorruptTheMemory(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a")), beat(2, 20, link("b"))})
	id := testIdentity(t)
	state := filepath.Join(t.TempDir(), "state.json")
	w := NewWatcher(feed.srv.URL, state, id, feed.srv.Client())
	var mu sync.Mutex
	corrupt := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				_, _, err := w.CheckOnce(context.Background())
				if err == nil {
					continue
				}
				msg := err.Error()
				// A rename losing a race is a benign miss; a memory that will not parse or will not
				// verify is the witness reporting its own testimony as forged.
				if strings.Contains(msg, "parse checkpoint") || strings.Contains(msg, "verify") {
					mu.Lock()
					corrupt[msg]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if len(corrupt) != 0 {
		t.Errorf("overlapping watches produced %v; a witness must never report its own memory as "+
			"altered because two of its own polls overlapped", corrupt)
	}
	if _, err := w.Checkpoint(); err != nil {
		t.Errorf("the state file is unreadable after overlapping watches: %v", err)
	}
}

// TestClipRefusesANegativeLimitRatherThanPanicking pins that the one bound standing between an
// untrusted feed's text and the findings record cannot be made to crash the process.
//
// clip backs the cut off a rune boundary with a loop guarded on cut > 0, then slices at cut. A
// negative limit skips the loop entirely and slices at a negative index, which panics. No caller
// passes a negative limit today, so this is latent rather than live, but the function is the
// package's shared bound and a witness that panics is a witness that stops watching.
func TestClipRefusesANegativeLimitRatherThanPanicking(t *testing.T) {
	t.Parallel()
	if got := clip("hello", -1); got != "..." && got != "" {
		t.Errorf("clip(%q, -1) = %q, want a bounded value rather than a panic", "hello", got)
	}
}
