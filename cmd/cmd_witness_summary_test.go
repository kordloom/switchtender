package cmd

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/witness"
)

// TestWitnessSummaryNamesWhatTheWitnessHolds pins the closing line of a clean --once check: the
// beat, chain position, and head the witness now holds, with the head cut to a readable prefix.
func TestWitnessSummaryNamesWhatTheWitnessHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Checkpoint *witness.Checkpoint
		Want       string
	}{{ // Test 0: A full checkpoint names its beat, position, and a twelve-character head.
		Checkpoint: &witness.Checkpoint{LastBeat: 161, LastSeq: 212,
			LastHead: "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b"},
		Want: "witness: no findings. Checkpoint at beat 161, chain position 212, head 1a2b3c4d5e6f.",
	}, { // Test 1: A head shorter than the prefix is shown whole.
		Checkpoint: &witness.Checkpoint{LastBeat: 1, LastSeq: 2, LastHead: "abc"},
		Want:       "witness: no findings. Checkpoint at beat 1, chain position 2, head abc.",
	}, { // Test 2: No checkpoint still says the check was clean.
		Checkpoint: nil,
		Want:       "witness: no findings.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := witnessSummary(test.Checkpoint); got != test.Want {
				t.Errorf("witnessSummary() = %q, want %q", got, test.Want)
			}
		})
	}
}
