package server

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestParseFieldedQuery pins the query grammar, including the two terms that turn dead ends into
// links and the quoting that makes a multi-word value addressable at all.
//
// An approval rule is named in prose, so held_by:"prod terraform destroy" has to survive parsing
// as one term. Splitting on spaces alone shredded it into free text, and the deep link from a
// policy's Holding count could never say which rule it meant.
func TestParseFieldedQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want run.ListFilter
	}{{ // Test 0: The worker term fills ClaimedBy.
		In: "worker:web-01", Want: run.ListFilter{ClaimedBy: "web-01"},
	}, { // Test 1: A quoted held_by value survives as one term.
		In:   `held_by:"prod terraform destroy" status:pending_approval`,
		Want: run.ListFilter{HeldBy: "prod terraform destroy", Status: "pending_approval"},
	}, { // Test 2: An unquoted single-word rule still works.
		In: "held_by:freeze", Want: run.ListFilter{HeldBy: "freeze"},
	}, { // Test 3: Quotes around free text group it without inventing a field.
		In: `"two words" tool:terraform`, Want: run.ListFilter{Query: "two words", Tool: "terraform"},
	}, { // Test 4: The old grammar is unchanged.
		In:   "status:failed actor:root deploy",
		Want: run.ListFilter{Status: "failed", Actor: "root", Query: "deploy"},
	}, { // Test 5: An unterminated quote degrades to taking the rest of the string, not a panic.
		In: `held_by:"half open`, Want: run.ListFilter{HeldBy: "half open"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got run.ListFilter
			parseFieldedQuery(test.In, &got)
			if got != test.Want {
				t.Errorf("parse(%q) = %+v, want %+v", test.In, got, test.Want)
			}
		})
	}
}
