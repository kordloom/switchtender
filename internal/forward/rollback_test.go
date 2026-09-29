package forward

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// chainOf builds a chain from paths, one entry each. Entries are fixed in id and time, so two chains
// built from the same leading paths share those entries link for link, the way a database restored
// from an older copy shares its history up to the copy.
func chainOf(t *testing.T, paths ...string) audit.Store {
	t.Helper()
	audits := audit.NewMemStore()
	base := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	for i, p := range paths {
		e := &audit.Entry{ID: fmt.Sprintf("aud_%d_%s", i, p), At: base.Add(time.Duration(i) * time.Second),
			Actor: "op", Method: "POST", Path: p}
		if err := audits.Append(context.Background(), e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return audits
}

// drain forwards until the chain is exhausted, three entries a batch, so the cursor passes through
// checkpoints on the way.
func drain(t *testing.T, audits audit.Store, cursor string) []Event {
	t.Helper()
	sink := &captureSink{}
	f := NewForwarder(audits, []Sink{sink}, cursor, time.Second, nil)
	f.batch = 3
	for {
		n, err := f.forwardOnce(context.Background())
		if err != nil {
			t.Fatalf("forwardOnce() error = %v", err)
		}
		if n == 0 {
			return sink.all()
		}
	}
}

// paths lists the paths of events in order.
func paths(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Path)
	}
	return out
}

// TestAForwarderSendsWhatARolledBackChainAppended pins what the forwarder does when the chain under
// its cursor was rolled back, by restoring an older copy of the database or by cutting its tail. The
// cursor stayed ahead of the chain, so every entry the rolled-back chain appended at a sequence the
// cursor had passed was never sent, and nothing said so. The forwarder resumes after the newest
// delivered entry the chain still holds, so the new entries reach the SIEM.
func TestAForwarderSendsWhatARolledBackChainAppended(t *testing.T) {
	t.Parallel()
	original := []string{"/a1", "/a2", "/a3", "/a4", "/a5", "/a6", "/a7", "/a8", "/a9", "/a10"}
	tests := []struct {
		Name      string
		After     []string
		WantPaths []string
	}{{ // Test 0: Restored to after entry six, then four new entries.
		Name: "restored", After: append(append([]string{}, original[:6]...), "/n7", "/n8", "/n9", "/n10"),
		WantPaths: []string{"/n7", "/n8", "/n9", "/n10"},
	}, { // Test 1: The tail cut to five entries, below the checkpoint at six, then one new entry.
		Name: "cut", After: append(append([]string{}, original[:5]...), "/n6"),
		WantPaths: []string{"/a4", "/a5", "/n6"},
	}, { // Test 2: Nothing changed, so nothing is sent again.
		Name: "unchanged", After: original, WantPaths: []string{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cursor := filepath.Join(t.TempDir(), "cursor.json")
			if got := drain(t, chainOf(t, original...), cursor); len(got) != len(original) {
				t.Fatalf("%s: first pass delivered %d entries, want %d", test.Name, len(got), len(original))
			}
			got := paths(drain(t, chainOf(t, test.After...), cursor))
			if fmt.Sprint(got) != fmt.Sprint(test.WantPaths) {
				t.Errorf("%s: after the chain changed the forwarder sent %v, want %v", test.Name, got,
					test.WantPaths)
			}
		})
	}
}

// TestACursorWithoutALinkStillNoticesAShortenedChain pins the cursor files written before links were
// kept. One that points past the end of the chain cannot be trusted, so the chain is sent again from
// its start, and the receipt on each entry lets the SIEM drop what it already holds.
func TestACursorWithoutALinkStillNoticesAShortenedChain(t *testing.T) {
	t.Parallel()
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	if err := os.WriteFile(cursor, []byte(`{"seq":10}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	got := paths(drain(t, chainOf(t, "/a1", "/a2", "/n3"), cursor))
	if fmt.Sprint(got) != fmt.Sprint([]string{"/a1", "/a2", "/n3"}) {
		t.Errorf("a cursor past the end of the chain sent %v, want the chain from its start", got)
	}
}

// TestCheckpointsReachBackAsTheyThin pins that the bounded checkpoint list covers more than the last
// few batches. Keeping only the newest positions meant a restore from yesterday's copy matched none of
// them and sent the whole chain again.
func TestCheckpointsReachBackAsTheyThin(t *testing.T) {
	t.Parallel()
	var c cursorDoc
	for seq := int64(1); seq <= 5000; seq++ {
		c = c.advance(seq, fmt.Sprintf("link%d", seq))
	}
	if len(c.Checkpoints) > maxCheckpoints {
		t.Errorf("kept %d checkpoints, want at most %d", len(c.Checkpoints), maxCheckpoints)
	}
	if oldest := c.Checkpoints[0].Seq; oldest > 5000-10*maxCheckpoints {
		t.Errorf("the oldest checkpoint is seq %d, so a rollback further back than the last few "+
			"hundred entries matches none of them", oldest)
	}
	for i := 1; i < len(c.Checkpoints); i++ {
		if c.Checkpoints[i].Seq <= c.Checkpoints[i-1].Seq {
			t.Fatalf("checkpoints out of order at %d: %v", i, c.Checkpoints)
		}
	}
}
