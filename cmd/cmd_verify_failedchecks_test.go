package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestFailedChecksNamesOnlyTheChecksThatApply pins that a refusal reports the problems the receipt
// actually has.
//
// Anchors are not trusted over a chain that does not recompute, so a broken chain marks them failed
// as well. A receipt carrying no anchor was therefore refused with the reason that an anchor names
// a position it does not prove, which is not true of a receipt with no anchors and points a reader
// at evidence that was never there.
func TestFailedChecksNamesOnlyTheChecksThatApply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Report     *audit.BundleReport
		WantHas    []string
		WantHasNot []string
	}{{ // Test 0: A broken chain on a receipt carrying no anchor says nothing about anchors.
		Name: "broken chain without anchors",
		Report: &audit.BundleReport{
			SignatureOK: false, ChainOK: false, BrokeAtSeq: 1,
			AnchorsOK: false, AnchorCount: 0,
			DecisionsOK: true, SpecConsistent: true,
		},
		WantHas:    []string{"the signature does not cover these bytes", "does not recompute at seq 1"},
		WantHasNot: []string{"anchor"},
	}, { // Test 1: The same receipt carrying an anchor does report it, since the anchor is real and
		// is genuinely not proven by a chain that does not recompute.
		Name: "broken chain with an anchor",
		Report: &audit.BundleReport{
			SignatureOK: true, ChainOK: false, BrokeAtSeq: 3,
			AnchorsOK: false, AnchorCount: 1,
			DecisionsOK: true, SpecConsistent: true,
		},
		WantHas:    []string{"an anchor names a position this receipt does not prove"},
		WantHasNot: []string{"the signature does not cover"},
	}, { // Test 2: A timestamp problem is named specifically rather than as a generic anchor fault.
		Name: "timestamp token problem",
		Report: &audit.BundleReport{
			SignatureOK: true, ChainOK: true,
			AnchorsOK: false, AnchorCount: 1, TimestampProblems: []string{"bad token"},
			DecisionsOK: true, SpecConsistent: true,
		},
		WantHas:    []string{"a timestamp token does not fix the link its anchor names"},
		WantHasNot: []string{"an anchor names a position"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := failedChecks(test.Report)
			for _, want := range test.WantHas {
				if !strings.Contains(got, want) {
					t.Errorf("reason %q does not mention %q", got, want)
				}
			}
			for _, unwanted := range test.WantHasNot {
				if strings.Contains(got, unwanted) {
					t.Errorf("reason %q mentions %q, which this receipt has no problem with", got, unwanted)
				}
			}
		})
	}
}
