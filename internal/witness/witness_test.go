package witness

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// beat builds one feed record.
func beat(n, seq int64, head string) Beat {
	return Beat{Beat: n, Seq: seq, Head: head, At: "2026-08-01T00:00:00Z"}
}

func TestCheckCleanStreamRaisesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	first, findings := Check(nil, "https://st.example", []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")}, now)
	if len(findings) != 0 {
		t.Fatalf("first watch findings = %v, want none", findings)
	}
	if first.LastBeat != 2 || first.LastSeq != 20 || first.LastHead != "bb" {
		t.Errorf("checkpoint = %+v, want the newest beat remembered", first)
	}
	next, findings := Check(first, "https://st.example", []Beat{beat(2, 20, "bb"), beat(3, 30, "cc")}, now)
	if len(findings) != 0 {
		t.Fatalf("advancing watch findings = %v, want none", findings)
	}
	if next.LastBeat != 3 {
		t.Errorf("checkpoint last beat = %d, want 3", next.LastBeat)
	}
}

func TestCheckFindsAGapInTheFeed(t *testing.T) {
	t.Parallel()
	_, findings := Check(nil, "s", []Beat{beat(1, 10, "aa"), beat(4, 40, "dd")}, time.Now())
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
	first, _ := Check(nil, "s", []Beat{beat(1, 10, "aa")}, now)
	_, findings := Check(first, "s", []Beat{beat(1, 10, "ALTERED")}, now)
	if len(findings) != 1 || findings[0].Kind != "rewritten_beat" {
		t.Fatalf("findings = %v, want one rewritten_beat", findings)
	}
}

func TestCheckFindsAHeadRegression(t *testing.T) {
	t.Parallel()
	now := time.Now()
	first, _ := Check(nil, "s", []Beat{beat(1, 10, "aa"), beat(2, 20, "bb"), beat(3, 30, "cc")}, now)

	// Test 0: the newest served beat is older than one already witnessed.
	_, findings := Check(first, "s", []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")}, now)
	if len(findings) != 1 || findings[0].Kind != "head_regression" {
		t.Fatalf("truncated feed findings = %v, want one head_regression", findings)
	}

	// Test 1: the feed goes empty after beats were witnessed.
	_, findings = Check(first, "s", nil, now)
	if len(findings) != 1 || findings[0].Kind != "head_regression" {
		t.Fatalf("empty feed findings = %v, want one head_regression", findings)
	}
}

func TestCheckpointSignatureRoundTripAndTamper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	path := filepath.Join(dir, "state.json")
	c, _ := Check(nil, "https://st.example", []Beat{beat(1, 10, "aa")}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err := Save(path, c, id); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(path)
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
	none, err := Load(filepath.Join(dir, "absent.json"))
	if err != nil || none != nil {
		t.Errorf("Load(absent) = %v, %v, want nil, nil", none, err)
	}
}
