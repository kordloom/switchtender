package decision

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestReasonPathsRoundTrip pins the chain paths a correction and a redaction are recorded at, and
// that nothing else reads as one: a decision's own path, a step's, or a near miss stays an ordinary
// entry rather than becoming a redaction the record never made.
func TestReasonPathsRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Path is what the chain holds.
		Path string
		// WantEntry is what it reads back as.
		WantEntry ReasonEntry
		// WantOK is whether it is a reason path at all.
		WantOK bool
	}{{ // Test 0: A correction.
		Path:      CorrectionPath("run_a", "aud_d", "aud_c"),
		WantEntry: ReasonEntry{RunID: "run_a", DecisionID: "aud_d", CorrectionID: "aud_c"},
		WantOK:    true,
	}, { // Test 1: A decision's reason redacted.
		Path: RedactionPath("run_a", "aud_d", "", CategoryPersonalData),
		WantEntry: ReasonEntry{RunID: "run_a", DecisionID: "aud_d", Redacted: true,
			Category: CategoryPersonalData},
		WantOK: true,
	}, { // Test 2: A correction's text redacted.
		Path: RedactionPath("run_a", "aud_d", "aud_c", CategorySecret),
		WantEntry: ReasonEntry{RunID: "run_a", DecisionID: "aud_d", CorrectionID: "aud_c",
			Redacted: true, Category: CategorySecret},
		WantOK: true,
	}, { // Test 3: A decision's own path is not one.
		Path: "/runs/run_a/decision/approved", WantOK: false,
	}, { // Test 4: An unknown category is not one.
		Path: "/runs/run_a/decisions/aud_d/reason_redacted/whatever", WantOK: false,
	}, { // Test 5: A decision named with nothing after it is not one.
		Path: "/runs/run_a/decisions/aud_d", WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, ok := ParseReasonPath(test.Path)
			if ok != test.WantOK {
				t.Fatalf("ParseReasonPath(%q) ok = %v, want %v", test.Path, ok, test.WantOK)
			}
			if diff := cmp.Diff(test.WantEntry, got); diff != "" {
				t.Errorf("entry mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
