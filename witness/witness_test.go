package witness

import (
	"fmt"
	"strconv"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/identity"
)

// beat builds one feed record.
func beat(n, seq int64, head string) Beat {
	return Beat{Beat: n, Seq: seq, Head: head, At: "2026-08-01T00:00:00Z"}
}

func TestCheckCleanStreamRaisesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	first, findings, _ := Check(nil, "https://st.example", []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")}, now)
	if len(findings) != 0 {
		t.Fatalf("first watch findings = %v, want none", findings)
	}
	if first.LastBeat != 2 || first.LastSeq != 20 || first.LastHead != "bb" {
		t.Errorf("checkpoint = %+v, want the newest beat remembered", first)
	}
	next, findings, _ := Check(first, "https://st.example", []Beat{beat(2, 20, "bb"), beat(3, 30, "cc")}, now)
	if len(findings) != 0 {
		t.Fatalf("advancing watch findings = %v, want none", findings)
	}
	if next.LastBeat != 3 {
		t.Errorf("checkpoint last beat = %d, want 3", next.LastBeat)
	}
}

func TestCheckFindsAGapInTheFeed(t *testing.T) {
	t.Parallel()
	_, findings, _ := Check(nil, "s", []Beat{beat(1, 10, "aa"), beat(4, 40, "dd")}, time.Now())
	if len(findings) != 1 || findings[0].Kind != "missing_beat" {
		t.Fatalf("findings = %v, want one missing_beat", findings)
	}
	if !strings.Contains(findings[0].Detail, "2 beat(s)") {
		t.Errorf("detail = %q, want the count of missing beats", findings[0].Detail)
	}
}

func TestCheckFindsARewrittenBeat(t *testing.T) {
	t.Parallel()
	now := time.Now()
	first, _, _ := Check(nil, "s", []Beat{beat(1, 10, "aa")}, now)
	_, findings, _ := Check(first, "s", []Beat{beat(1, 10, "ALTERED")}, now)
	if len(findings) != 1 || findings[0].Kind != "rewritten_beat" {
		t.Fatalf("findings = %v, want one rewritten_beat", findings)
	}
}

func TestCheckFindsAHeadRegression(t *testing.T) {
	t.Parallel()
	now := time.Now()
	first, _, _ := Check(nil, "s", []Beat{beat(1, 10, "aa"), beat(2, 20, "bb"), beat(3, 30, "cc")}, now)

	// Test 0: the newest served beat is older than one already witnessed.
	_, findings, _ := Check(first, "s", []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")}, now)
	if len(findings) != 1 || findings[0].Kind != "head_regression" {
		t.Fatalf("truncated feed findings = %v, want one head_regression", findings)
	}

	// Test 1: the feed goes empty after beats were witnessed.
	_, findings, _ = Check(first, "s", nil, now)
	if len(findings) != 1 || findings[0].Kind != "head_regression" {
		t.Fatalf("empty feed findings = %v, want one head_regression", findings)
	}
}

func TestCheckpointSignatureRoundTripAndTamper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	path := filepath.Join(dir, "state.json")
	c, _, _ := Check(nil, "https://st.example", []Beat{beat(1, 10, "aa")}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err := Save(path, c, id); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(path, id.PublicKeyHex())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.LastBeat != 1 || got.Server != "https://st.example" {
		t.Errorf("loaded checkpoint = %+v, want what was saved", got)
	}

	// A state file altered on disk must be refused, or the witness's memory can be rewritten
	// under it, which is the exact attack it exists to catch.
	tampered := *got
	tampered.LastBeat = 99
	if _, err := Verify(&tampered); err == nil {
		t.Fatal("Verify() accepted an altered checkpoint")
	}

	// A missing file is the first watch, not an error.
	none, err := Load(filepath.Join(dir, "absent.json"), id.PublicKeyHex())
	if err != nil || none != nil {
		t.Errorf("Load(absent) = %v, %v, want nil, nil", none, err)
	}
}

func TestCheckRemembersEveryBeatTheFeedStillServes(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// A full feed window, then the same window again with the oldest beat rewritten. A witness
	// that remembers less than the feed serves forgets that beat and re-adopts the rewrite.
	full := make([]Beat, 0, FeedLimit)
	for i := int64(1); i <= FeedLimit; i++ {
		full = append(full, beat(i, i*10, fmt.Sprintf("head%d", i)))
	}
	first, findings, err := Check(nil, "s", full, now)
	if err != nil || len(findings) != 0 {
		t.Fatalf("first watch = %v, %v, want a clean baseline", findings, err)
	}
	if len(first.Recent) != FeedLimit {
		t.Fatalf("remembered %d of %d served beats; a forgotten beat's rewrite is invisible",
			len(first.Recent), FeedLimit)
	}
	rewritten := make([]Beat, len(full))
	copy(rewritten, full)
	rewritten[0] = beat(1, 10, "REWRITTEN")
	_, findings, err = Check(first, "s", rewritten, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != "rewritten_beat" {
		t.Fatalf("findings = %v, want the oldest still-served beat reported as rewritten", findings)
	}
}

func TestCheckNeverAdoptsARewrittenHeadIntoItsOwnTestimony(t *testing.T) {
	t.Parallel()
	now := time.Now()
	first, _, _ := Check(nil, "s", []Beat{beat(1, 10, "aa"), beat(2, 20, "GENUINE")}, now)
	next, findings, err := Check(first, "s", []Beat{beat(1, 10, "aa"), beat(2, 20, "FORGED")}, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != "rewritten_beat" {
		t.Fatalf("findings = %v, want one rewritten_beat", findings)
	}
	// The checkpoint is the witness's testimony. Signing the forged head into it would have the
	// witness endorse the very rewrite it is reporting.
	if next.LastHead != "GENUINE" {
		t.Errorf("checkpoint head = %q, want the head first witnessed, not the rewrite", next.LastHead)
	}
}

func TestCheckRefusesACheckpointFromAnotherServer(t *testing.T) {
	t.Parallel()
	now := time.Now()
	first, _, _ := Check(nil, "https://a.example", []Beat{beat(1, 10, "aa")}, now)
	_, _, err := Check(first, "https://b.example", []Beat{beat(1, 99, "zz")}, now)
	if err == nil {
		t.Fatal("Check() held one server's memory against another's feed, inventing findings and " +
			"overwriting the memory that would catch a real rewrite")
	}
}

func TestLoadRefusesACheckpointSignedByAnotherKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mine, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	// A forger with write access to the state file generates their own key, writes a checkpoint
	// whose memory matches the truncated feed, and signs it. It is internally consistent.
	forgerDir := t.TempDir()
	forger, err := identity.Load(forgerDir)
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	path := filepath.Join(dir, "state.json")
	forged, _, _ := Check(nil, "s", []Beat{beat(1, 10, "aa")}, time.Now())
	if err := Save(path, forged, forger); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := Load(path, mine.PublicKeyHex()); err == nil {
		t.Fatal("Load() accepted a checkpoint signed by a key that is not this witness's, so a " +
			"replaced state file wipes the memory a truncation would have been caught by")
	}
}

func TestCheckAcceptsTheSameServerSpelledWithATrailingSlash(t *testing.T) {
	t.Parallel()
	now := time.Now()
	first, _, _ := Check(nil, "https://st.example", []Beat{beat(1, 10, "aa")}, now)
	// The fetch normalizes the base URL, so refusing this spelling blinds the witness forever
	// while it reports itself healthy.
	_, _, err := Check(first, "https://st.example/", []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")}, now)
	if err != nil {
		t.Fatalf("Check() refused the same server spelled with a trailing slash: %v", err)
	}
}

func TestCheckDoesNotAdoptAHeadBuiltOnARewrite(t *testing.T) {
	t.Parallel()
	now := time.Now()
	first, _, _ := Check(nil, "s", []Beat{beat(1, 10, "aa"), beat(2, 20, "GENUINE")}, now)
	// Beat 2 is rewritten and beat 3 is appended on top of the rewritten history. Beat 3 looks
	// new, but every beat after a rewrite belongs to the forged chain.
	next, findings, err := Check(first, "s",
		[]Beat{beat(1, 10, "aa"), beat(2, 20, "FORGED"), beat(3, 30, "onForged")}, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != "rewritten_beat" {
		t.Fatalf("findings = %v, want the rewrite reported", findings)
	}
	if next.LastHead == "onForged" {
		t.Error("the witness signed a head built on the rewrite into its own testimony")
	}
}

// TestWholeChainReplacementUnderFreshBeatNumbersIsCaught proves the hole an adversarial review
// reproduced: rewrite detection keyed on the beat number, so an operator who replaced the entire
// chain and served it under beat numbers the witness had never seen was adopted in silence and
// attested clean. The chain position is the coordinate the watched server cannot renumber, and a
// chain only appends, so a newest position behind the witnessed one is a replaced history.
func TestWholeChainReplacementUnderFreshBeatNumbersIsCaught(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	// The witness watches a healthy server: beats 1..3 at chain positions 1..3.
	first, findings, err := Check(nil, "https://st.example",
		[]Beat{beat(1, 1, "aa01"), beat(2, 2, "aa02"), beat(3, 3, "aa03")}, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("first watch = %v, want a clean baseline", findings)
	}

	// The operator replaces the whole chain and serves it under beat numbers 4..6, so no beat the
	// witness remembers is ever presented again. The chain is now three entries long, so its
	// newest position is 3 while the witness has already seen position 3 on a different history.
	_, findings, err = Check(first, "https://st.example",
		[]Beat{beat(4, 1, "bb01"), beat(5, 2, "bb02"), beat(6, 3, "bb03")}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	var caught bool
	for _, f := range findings {
		if f.Kind == "rewritten_history" || f.Kind == "seq_regression" {
			caught = true
		}
	}
	if !caught {
		t.Errorf("a whole-chain replacement under fresh beat numbers raised %v, want the "+
			"replacement named", findings)
	}
}

// TestGapAcrossPollsIsCaught proves a feed that jumps past the witnessed beat between polls is
// reported. The gap walk only ever compared beats inside one answer, so the entries between the
// witnessed beat and the oldest beat of the next answer went missing in silence.
func TestGapAcrossPollsIsCaught(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	first, _, err := Check(nil, "https://st.example",
		[]Beat{beat(1, 1, "aa01"), beat(2, 2, "aa02")}, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	_, findings, err := Check(first, "https://st.example",
		[]Beat{beat(9, 9, "aa09"), beat(10, 10, "aa10")}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	var sawMissing bool
	for _, f := range findings {
		if f.Kind == "missing_beat" {
			sawMissing = true
		}
	}
	if !sawMissing {
		t.Errorf("a cross-poll gap raised %v, want missing_beat", findings)
	}
}

// TestImplausibleBeatsAreRefusedNotRemembered proves a beat the witness cannot check is kept out of
// signed memory and named, rather than adopted and wedging the checkpoint at whatever it claims.
func TestImplausibleBeatsAreRefusedNotRemembered(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	cp, findings, err := Check(nil, "https://st.example", []Beat{
		beat(1, 1, "aa01"),
		{Beat: 1 << 62, Seq: 5, Head: "aa02", At: "2026-08-01T00:00:00Z"},
		{Beat: 3, Seq: 0, Head: "aa03", At: "2026-08-01T00:00:00Z"},
		{Beat: 4, Seq: 4, Head: "", At: "2026-08-01T00:00:00Z"},
	}, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	var sawMalformed bool
	for _, f := range findings {
		if f.Kind == "malformed_feed" {
			sawMalformed = true
		}
	}
	if !sawMalformed {
		t.Errorf("findings = %v, want malformed_feed", findings)
	}
	if cp.LastBeat != 1 {
		t.Errorf("checkpoint adopted beat %d, want the only plausible beat 1", cp.LastBeat)
	}
}

// TestOneHostileFeedDoesNotBecomeAThousandFindings proves the gap walk is summarized. A feed can
// serve a thousand beats each one past the last, and reporting a finding per gap turned one poll
// into a thousand disk records and a thousand webhook deliveries.
func TestOneHostileFeedDoesNotBecomeAThousandFindings(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	beats := make([]Beat, 0, 500)
	for i := int64(0); i < 500; i++ {
		beats = append(beats, beat(1+i*2, 1+i*2, "aa"+strconv.FormatInt(i, 16)))
	}
	_, findings, err := Check(nil, "https://st.example", beats, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) > 2 {
		t.Errorf("a feed with 499 gaps raised %d findings, want them summarized", len(findings))
	}
	if len(findings) == 0 {
		t.Error("a feed with 499 gaps raised nothing")
	}
}
