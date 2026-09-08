package witness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests close holes found by mutating the source and watching the rest of the suite stay
// green. Each one fails against a specific realistic defect that nothing else caught.

// TestGapFindingNamesTheRangeInTheRightOrder pins the two beat numbers the gap summary reports.
//
// The gap walk is summarized rather than reported per gap, so the only thing an operator can act on
// is the range in its wording. Swapping the oldest and the newest beat in that sentence tells them
// to look between beat 10 and beat 1, which is a range that does not exist, and every other
// assertion in the suite passes because the kind and the counts are unchanged.
func TestGapFindingNamesTheRangeInTheRightOrder(t *testing.T) {
	t.Parallel()
	beats := []Beat{beat(1, 1, link("a")), beat(2, 2, link("b")), beat(10, 10, link("j"))}
	_, findings, err := Check(nil, "https://st.example", beats, fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	var detail string
	for _, f := range findings {
		if f.Kind == "missing_beat" {
			detail = f.Detail
		}
	}
	if detail == "" {
		t.Fatal("a feed skipping beats 3 through 9 raised no missing_beat finding")
	}
	if want := "between beat 1 and beat 10"; !strings.Contains(detail, want) {
		t.Errorf("missing_beat detail = %q, want it to name the range %q, oldest first, so an "+
			"operator is sent to a range that exists", detail, want)
	}
}

// TestAttestationCountsRememberedBeatsNotChainPositions pins which of the checkpoint's two memories
// BeatsRemembered reports.
//
// The checkpoint holds beats keyed by the watched server's own counter and links keyed by chain
// position, and the two counts are equal for every honest feed, which is exactly why reporting the
// wrong one survives an honest test. They diverge when a chain is replaced wholesale and served
// under fresh beat numbers: the beats are all new, the positions are all ones already witnessed. A
// relying party reading "3 beats remembered" from a witness that holds six is being told this
// witness saw less than it did, in a signed statement, about the one event it exists to catch.
func TestAttestationCountsRememberedBeatsNotChainPositions(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, chainBeats(3))
	s, key := startedService(t, feed, t.TempDir(), testIdentity(t))

	// The same three chain positions, relabeled onto beat numbers this witness has never seen.
	replaced := []Beat{
		beat(4, 1, link("forged-1")), beat(5, 2, link("forged-2")), beat(6, 3, link("forged-3")),
	}
	feed.set(replaced, 0)
	s.CheckAll(context.Background())

	a, err := s.Attest(key)
	if err != nil || a == nil {
		t.Fatalf("Attest() = %+v, %v, want a signed statement", a, err)
	}
	c, err := s.watchers[key].Checkpoint()
	if err != nil || c == nil {
		t.Fatalf("Checkpoint() = %+v, %v", c, err)
	}
	if len(c.Recent) == len(c.Positions) {
		t.Fatalf("the memories did not diverge: %d beats and %d positions, so this test could not "+
			"tell the two apart", len(c.Recent), len(c.Positions))
	}
	if a.BeatsRemembered != len(c.Recent) {
		t.Errorf("attestation reports %d beats remembered, want %d, the beats the memory holds; "+
			"it is reporting %d chain positions instead", a.BeatsRemembered, len(c.Recent),
			len(c.Positions))
	}
}

// TestFindingsAreFilteredByTheWholeServerNotAPrefix pins the per-server findings filter.
//
// One watched server's findings are another operator's business, and the API hands them out by
// server. Matching on a prefix rather than the whole URL means a request for what was witnessed at
// one host also returns everything witnessed at every host whose URL extends it, which is a
// cross-tenant leak that no single-server test can see.
func TestFindingsAreFilteredByTheWholeServerNotAPrefix(t *testing.T) {
	t.Parallel()
	const mine, neighbor = "https://a.example", "https://a.example.com"
	dir := t.TempDir()
	var lines []byte
	for _, rec := range []RecordedFinding{
		{At: fixedClock, Server: mine, Kind: "rewritten_beat", Detail: "mine"},
		{At: fixedClock, Server: neighbor, Kind: "rewritten_beat", Detail: "not mine"},
	} {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		lines = append(append(lines, line...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, findingsFile), lines, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	s, err := NewService(testIdentity(t), dir, time.Minute, []string{mine, neighbor}, nil, nil)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer s.Close()

	recs, err := s.tail(mine)
	if err != nil {
		t.Fatalf("tail() error = %v", err)
	}
	if len(recs) != 1 || recs[0].Server != mine {
		t.Fatalf("tail(%q) returned %d record(s) %+v, want only the one recorded against that "+
			"server; a longer URL that starts with it is a different server", mine, len(recs), recs)
	}
}

// TestBlindnessIsClearedWhenTheWitnessCanSeeAgain pins that the reported error does not outlive the
// outage.
//
// Blind is cleared on recovery, so a stale LastError beside Blind false reads as a witness that is
// currently failing while its own edge tracker says it is fine. An operator triaging the listing
// cannot tell that error from a live one, and the contradiction is in a field an attestation's
// neighbors are read against.
func TestBlindnessIsClearedWhenTheWitnessCanSeeAgain(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, chainBeats(2))
	feed.set(nil, http.StatusInternalServerError)
	s, _ := startedService(t, feed, t.TempDir(), testIdentity(t))

	states := s.states()
	if len(states) != 1 || !states[0].Blind || states[0].LastError == "" {
		t.Fatalf("state after a dark feed = %+v, want blind with the reason recorded", states)
	}
	feed.set(chainBeats(2), 0)
	s.CheckAll(context.Background())

	states = s.states()
	if len(states) != 1 {
		t.Fatalf("states() = %+v, want one server", states)
	}
	if states[0].Blind {
		t.Fatalf("the witness still reports itself blind after a successful check: %+v", states[0])
	}
	if states[0].LastError != "" {
		t.Errorf("last error = %q after the witness could see again, want it cleared; a stale "+
			"reason beside blind=false reads as a failure that is not happening",
			states[0].LastError)
	}
}

// TestAFeedThatStaysEmptyKeepsSayingEmptyFeed pins which finding a second empty poll raises.
//
// The first poll against a server whose span beat was never turned on saves a checkpoint that
// remembers nothing, and every poll after it holds that memory against another empty answer.
// Nothing was witnessed, so nothing was lost, and reporting the tail as regressed sends the
// operator hunting a truncation instead of starting the beat writer the finding is meant to name.
func TestAFeedThatStaysEmptyKeepsSayingEmptyFeed(t *testing.T) {
	t.Parallel()
	first, findings, err := Check(nil, "https://st.example", nil, fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !hasKind(findings, "empty_feed") {
		t.Fatalf("first poll of an empty feed raised %v, want empty_feed", kindsOf(findings))
	}
	_, findings, err = Check(first, "https://st.example", nil, fixedClock.Add(time.Hour))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if hasKind(findings, "head_regression") {
		t.Errorf("a feed that has always been empty reported %v; nothing was ever witnessed, so "+
			"no tail was lost", kindsOf(findings))
	}
	if !hasKind(findings, "empty_feed") {
		t.Errorf("second poll of an empty feed raised %v, want empty_feed again so a nightly "+
			"witness keeps naming the configuration to fix", kindsOf(findings))
	}
	// A witness whose memory is genuinely non-empty must still report the tail as lost.
	held, _, err := Check(nil, "https://st.example", chainBeats(2), fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	_, findings, err = Check(held, "https://st.example", nil, fixedClock.Add(time.Hour))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !hasKind(findings, "head_regression") {
		t.Errorf("an empty feed behind %d witnessed beats raised %v, want head_regression",
			held.LastBeat, kindsOf(findings))
	}
}

// TestAttestationNamesTheWatchedServerNotTheMemorysOwnLabel pins where the subject of a signed
// statement comes from.
//
// Start loads whatever checkpoint sits at a server's state path, and the pin it applies is on the
// signer, not on the subject: a file this witness signed for one server, moved or renamed into
// another server's slot, verifies. Every later poll refuses it and the witness goes blind, which is
// correct, but the attestation is minted from the memory the process still holds. Taking the
// subject from that memory means this witness signs a statement about a server it does not watch,
// and the two spellings agree for every honest deployment, so nothing else notices.
func TestAttestationNamesTheWatchedServerNotTheMemorysOwnLabel(t *testing.T) {
	t.Parallel()
	const elsewhere = "https://elsewhere.example"
	feed := newFakeFeed(t, chainBeats(2))
	dir, id := t.TempDir(), testIdentity(t)

	// A memory of another server, signed by this witness, sitting in this server's slot.
	stray, _, err := Check(nil, elsewhere, chainBeats(2), fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if err := Save(filepath.Join(dir, StateFileName(feed.srv.URL)), stray, id); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	s, key := startedService(t, feed, dir, id)

	a, err := s.Attest(key)
	if err != nil || a == nil {
		t.Fatalf("Attest() = %+v, %v, want a signed statement", a, err)
	}
	if want := NormalizeServer(feed.srv.URL); a.Server != want {
		t.Errorf("attestation names %q, want the watched server %q; a witness must not sign a "+
			"statement about a server it does not watch", a.Server, want)
	}
	if a.Server == elsewhere {
		t.Errorf("the attestation took its subject from the memory's own label, so a state file " +
			"dropped into the wrong slot renames what this witness swears to")
	}
}

// TestAStoppedWitnessStopsSweepingEveryServer pins that a canceled sweep abandons the servers it
// has not reached.
//
// Dropping the cancellation check from the sweep loop means a stopped service still visits every
// watched server, stamps a fresh check time onto each, and turns the shutdown itself into a blind
// finding and an alert per server: an operator taking a witness down would be paged by taking it
// down.
//
// The assertion is deliberately the weak one, that not every server was reached, because the check
// as written cannot support the strong one. It is a select between sending to a buffered semaphore
// and a closed done channel, and while the semaphore has room both cases are ready, so the runtime
// picks between them at random: each server is abandoned on a coin flip rather than because the
// context is done. Over enough servers "every one was reached anyway" is what separates a guard
// that misfires from no guard at all, and that is what this pins. See the report accompanying this
// file for the underlying defect.
func TestAStoppedWitnessStopsSweepingEveryServer(t *testing.T) {
	t.Parallel()
	// A dead port, so a sweep that does run fails at once rather than waiting on anything.
	servers := make([]string, 0, 64)
	for i := range cap(servers) {
		servers = append(servers, fmt.Sprintf("http://127.0.0.1:1/%d", i))
	}
	var mu sync.Mutex
	var alerts int
	s, err := NewService(testIdentity(t), t.TempDir(), time.Minute, servers, nil, nil,
		WithServiceClock(func() time.Time { return fixedClock }),
		WithServiceNotify(func(string, Finding) {
			mu.Lock()
			defer mu.Unlock()
			alerts++
		}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	// Cancel without ever starting, so no sweep has run and every check time is still zero.
	s.Close()
	s.CheckAll(s.ctx)

	swept := 0
	for _, st := range s.states() {
		if !st.LastCheck.IsZero() {
			swept++
		}
	}
	if swept == len(servers) {
		t.Errorf("a sweep on a stopped service checked all %d servers, want it to abandon the ones "+
			"it had not reached; a shutdown must not read as every watched server going dark at "+
			"once", swept)
	}
	mu.Lock()
	defer mu.Unlock()
	if alerts == len(servers) {
		t.Errorf("stopping the witness alerted on all %d servers", alerts)
	}
}
