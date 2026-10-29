package dossier

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestDecisionOfReadsOnlyACommittedDecision pins which chain entries the dossier and the register
// credit as approvals and rejections.
//
// The HTTP attempt the middleware records looks almost identical to the committed decision, but it
// is written whether or not the decision took. Reading attempts meant a self-approval the separation
// gate refused still printed "Approved by" the requester, naming the wrong person in the one
// document built for an auditor, and a later failed re-approve overwrote the true approver's name.
//
//nolint:funlen // Test function.
func TestDecisionOfReadsOnlyACommittedDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Entry is the chain entry to read.
		Entry *audit.Entry
		// WantID is the run the entry decided, empty when it decided none.
		WantID string
		// WantVerdict is the verdict, empty when the entry is ordinary activity.
		WantVerdict string
	}{
		{ // Test 0: The committed approval.
			Name:   "committed approval",
			Entry:  &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/approved"},
			WantID: "run_1", WantVerdict: "Approved",
		},
		{ // Test 1: The committed rejection.
			Name:   "committed rejection",
			Entry:  &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/rejected"},
			WantID: "run_1", WantVerdict: "Rejected",
		},
		{ // Test 2: The HTTP attempt is not a decision, whatever its path looks like.
			Name:  "approval attempt",
			Entry: &audit.Entry{Method: "POST", Path: "/runs/run_1/decision/approved"},
		},
		{ // Test 3: Nor is the route the interface calls.
			Name:  "approve route",
			Entry: &audit.Entry{Method: "POST", Path: "/v1/runs/run_1/approve"},
		},
		{ // Test 4: A verdict the register does not know is not read as either one, so an unknown
			// decision cannot be laundered into an approval.
			Name:  "unknown verdict",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/expired"},
		},
		{ // Test 5: A decision path naming no run decides nothing.
			Name:  "no run id",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs//decision/approved"},
		},
		{ // Test 6: A decision method whose path is not a decision path decides nothing.
			Name:  "not a decision path",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1"},
		},
		{ // Test 7: An empty entry decides nothing.
			Name:  "empty entry",
			Entry: &audit.Entry{},
		},
		{ // Test 8: The verdict is matched exactly, so a different case is not the same verdict.
			Name:  "wrong case verdict",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/Approved"},
		},
		{ // Test 9: A trailing segment past the verdict is not the verdict.
			Name:  "verdict with a suffix",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/approved/x"},
		},
		{ // Test 10: A run id carrying its own separator still resolves to the whole id, so the
			// decision is credited to the run it was actually made on.
			Name: "id with a slash",
			Entry: &audit.Entry{Method: audit.MethodDecision,
				Path: "/runs/run_1/child/decision/approved"},
			WantID: "run_1/child", WantVerdict: "Approved",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id, verdict := DecisionOf(test.Entry)
			if id != test.WantID || verdict != test.WantVerdict {
				t.Errorf("DecisionOf(%s) = (%q, %q), want (%q, %q)",
					test.Name, id, verdict, test.WantID, test.WantVerdict)
			}
		})
	}
}
