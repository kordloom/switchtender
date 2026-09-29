package witness_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/witness"
)

// beat builds one feed beat.
func beat(n, seq int64, head string, at time.Time) witness.Beat {
	return witness.Beat{Beat: n, Seq: seq, Head: head, At: at.UTC().Format(time.RFC3339)}
}

// kinds lists the finding kinds a check produced, for assertions that care about presence.
func kinds(findings []witness.Finding) string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Kind)
	}
	return strings.Join(out, ",")
}

// TestAnEmptyFeedIsNotAClearAttestation covers the witness's quietest failure. A server that emits no
// beats at all, because the span beat was never turned on or because its operator chose to serve an
// empty feed, produced no findings: the witness saved a signed checkpoint, printed nothing, and exited
// zero. A cron running it nightly reported success every night while witnessing nothing whatsoever,
// which is worse than no witness, because somebody is relying on it.
func TestAnEmptyFeedIsNotAClearAttestation(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// Test 0: The first watch of a server that serves nothing says so.
	next, findings, err := witness.Check(nil, "https://switchtender.example.com", nil, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("an empty feed produced no findings, so a witness that saw nothing signed a " +
			"checkpoint saying all was well")
	}
	if !strings.Contains(kinds(findings), "empty_feed") {
		t.Errorf("findings = %s, want an empty_feed finding", kinds(findings))
	}
	if next == nil {
		t.Fatal("Check() returned no checkpoint")
	}
	// The finding names what to do, since the ordinary cause is a server that never enabled beats.
	if d := findings[0].Detail; !strings.Contains(d, "beat") {
		t.Errorf("detail = %q, want it to name the beat feed", d)
	}

	// Test 1: A feed with beats is unaffected.
	beats := []witness.Beat{beat(1, 10, strings.Repeat("a", 64), now)}
	if _, findings, err := witness.Check(nil, "https://switchtender.example.com", beats, now); err != nil {
		t.Fatalf("Check() error = %v", err)
	} else if len(findings) != 0 {
		t.Errorf("a healthy first watch produced %s, want nothing", kinds(findings))
	}
}

// TestAFrozenFeedIsReported covers a feed that stopped moving. A server whose chain stopped appending,
// or one replaying a fixed answer, served the same newest beat forever. The witness compared each answer
// against its memory, found no contradiction, and attested clean: a frozen feed and a healthy quiet one
// were indistinguishable, which is exactly the state an operator who has stopped recording wants to be
// in.
func TestAFrozenFeedIsReported(t *testing.T) {
	t.Parallel()
	start := time.Now().Add(-48 * time.Hour)
	head := strings.Repeat("b", 64)
	beats := []witness.Beat{beat(7, 42, head, start)}

	first, findings, err := witness.Check(nil, "https://switchtender.example.com", beats, start)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("the first watch produced %s, want nothing", kinds(findings))
	}

	// Test 0: The same answer a few minutes later is not a finding. Chains are quiet between changes.
	_, findings, err = witness.Check(first, "https://switchtender.example.com", beats,
		start.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("a quiet five minutes produced %s, want nothing: a chain with no changes emits no "+
			"new beats", kinds(findings))
	}

	// Test 1: The same answer a day later is. A span beat is emitted on a schedule whether or not the
	// chain changed, so an unmoved feed after that long is a feed that stopped.
	_, findings, err = witness.Check(first, "https://switchtender.example.com", beats,
		start.Add(26*time.Hour))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !strings.Contains(kinds(findings), "stalled_feed") {
		t.Errorf("findings after a day of no movement = %s, want a stalled_feed finding",
			kinds(findings))
	}

	// Test 2: A feed that moved on is healthy however long it took.
	moved := []witness.Beat{beat(8, 43, strings.Repeat("c", 64), start.Add(26*time.Hour))}
	_, findings, err = witness.Check(first, "https://switchtender.example.com", moved,
		start.Add(26*time.Hour))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("a feed that advanced produced %s, want nothing", kinds(findings))
	}
}

// TestAFrozenFeedIsReportedWhenTheWitnessPolls pins the stalled check under the way a witness is
// actually run: on a loop, feeding each checkpoint into the next.
//
// The existing frozen-feed test makes one twenty-six hour leap from the first observation, which
// passes whether or not the elapsed time accumulates. A real witness polls every few minutes and
// carries its checkpoint forward, and there the observation time was re-stamped on every poll that
// found the feed unmoved, so the comparison measured the gap between two consecutive polls rather
// than how long the head had stood still. That is always far below the staleness bound, so a frozen
// chain, a stopped beat writer, or a server replaying one fixed answer was signed as healthy for as
// long as anyone cared to look.
func TestAFrozenFeedIsReportedWhenTheWitnessPolls(t *testing.T) {
	t.Parallel()
	start := time.Now().Add(-72 * time.Hour)
	head := strings.Repeat("b", 64)
	beats := []witness.Beat{beat(7, 42, head, start)}
	const server = "https://switchtender.example.com"

	cp, findings, err := witness.Check(nil, server, beats, start)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("the first watch produced %s, want nothing", kinds(findings))
	}

	// Poll every five minutes for two days against a feed that never moves, carrying the checkpoint
	// forward exactly as the witness loop does.
	var sawStalled bool
	at := start
	for i := 0; i < 576 && !sawStalled; i++ {
		at = at.Add(5 * time.Minute)
		cp, findings, err = witness.Check(cp, server, beats, at)
		if err != nil {
			t.Fatalf("Check() at %v error = %v", at.Sub(start), err)
		}
		if strings.Contains(kinds(findings), "stalled_feed") {
			sawStalled = true
		}
	}
	if !sawStalled {
		t.Errorf("a feed frozen for %v across polling never raised stalled_feed, so a frozen or "+
			"replayed feed is attested healthy", at.Sub(start))
	}
	// And it fired once the feed had genuinely been still longer than the bound, not before.
	if elapsed := at.Sub(start); elapsed < witness.StaleAfter {
		t.Errorf("stalled_feed fired after only %v, under the %v bound: a quiet chain would be "+
			"reported as frozen", elapsed, witness.StaleAfter)
	}
}
