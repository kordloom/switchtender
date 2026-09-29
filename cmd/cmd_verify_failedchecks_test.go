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

// TestSpecVerdictNamesOnlyTheDigestsItCompared pins the spec line of a verified receipt. It named the
// approved, executed, and disclosed digests on every receipt, so a run that was rejected and never
// ran was reported as one whose approved and executed specs agreed. A rejected run still carries an
// outcome record, the rejection's, so the outcome is named by the status it committed.
func TestSpecVerdictNamesOnlyTheDigestsItCompared(t *testing.T) {
	t.Parallel()
	approved := []audit.DisclosedDecision{{Verdict: "approved"}}
	rejected := []audit.DisclosedDecision{{Verdict: "rejected"}}
	ended := func(status string) []byte { return []byte(`{"run_id":"run_1","status":"` + status + `"}`) }
	tests := []struct {
		Report      *audit.BundleReport
		WantVerdict string
		WantReason  string
	}{{ // Test 0: An approved run that executed names all three.
		Report: &audit.BundleReport{Decisions: approved, OutcomePresent: true, OutcomeDigestOK: true,
			OutcomeBody: ended("succeeded"), SpecPresent: true, SpecConsistent: true},
		WantVerdict: "approved, executed, and disclosed digests agree",
	}, { // Test 1: A rejected run's outcome is the rejection, so nothing is called approved or executed.
		Report: &audit.BundleReport{Decisions: rejected, OutcomePresent: true, OutcomeDigestOK: true,
			OutcomeBody: ended("rejected"), SpecPresent: true, SpecConsistent: true},
		WantVerdict: "rejected and disclosed digests agree",
	}, { // Test 2: A run that executed without a hold has no decision to name.
		Report: &audit.BundleReport{OutcomePresent: true, OutcomeDigestOK: true,
			OutcomeBody: ended("failed"), SpecPresent: true, SpecConsistent: true},
		WantVerdict: "executed and disclosed digests agree",
	}, { // Test 3: An approved run that has not finished names no outcome.
		Report:      &audit.BundleReport{Decisions: approved, SpecPresent: true, SpecConsistent: true},
		WantVerdict: "approved and disclosed digests agree",
	}, { // Test 4: A canceled run is named by its status, not as executed.
		Report: &audit.BundleReport{Decisions: approved, OutcomePresent: true, OutcomeDigestOK: true,
			OutcomeBody: ended("canceled"), SpecPresent: true, SpecConsistent: true},
		WantVerdict: "approved, canceled, and disclosed digests agree",
	}, { // Test 5: An outcome that did not verify is not compared, so it is not named.
		Report: &audit.BundleReport{Decisions: approved, OutcomePresent: true,
			OutcomeBody: ended("succeeded"), SpecPresent: true, SpecConsistent: true},
		WantVerdict: "approved and disclosed digests agree",
	}, { // Test 6: A disagreement between an approval and an execution says the change that ran differs.
		Report: &audit.BundleReport{Decisions: approved, OutcomePresent: true, OutcomeDigestOK: true,
			OutcomeBody: ended("succeeded"), SpecPresent: true},
		WantVerdict: "approved, executed, and disclosed digests do not agree, so the change that was " +
			"approved is not the change that ran",
		WantReason: "the approved and the executed change are not the same",
	}, { // Test 7: A disagreement on a rejected run does not claim anything was approved or ran.
		Report: &audit.BundleReport{Decisions: rejected, OutcomePresent: true, OutcomeDigestOK: true,
			OutcomeBody: ended("rejected"), SpecPresent: true},
		WantVerdict: "rejected and disclosed digests do not agree",
		WantReason:  "the spec digests this receipt discloses do not agree",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := specVerdict(test.Report); got != test.WantVerdict {
				t.Errorf("specVerdict() = %q, want %q", got, test.WantVerdict)
			}
			if test.WantReason == "" {
				return
			}
			test.Report.SignatureOK, test.Report.ChainOK, test.Report.DecisionsOK = true, true, true
			if got := failedChecks(test.Report); got != test.WantReason {
				t.Errorf("failedChecks() = %q, want %q", got, test.WantReason)
			}
		})
	}
}

// TestDecidedByNeverPrintsEmptyFields pins how a decision's actor reads. An install with no tokens
// records a decision with no actor, and the line printed "rejected by  (), binding spec ...".
func TestDecidedByNeverPrintsEmptyFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Decision audit.DisclosedDecision
		WantBy   string
	}{{ // Test 0: A named actor with how they authenticated.
		Decision: audit.DisclosedDecision{Actor: "drew", ActorType: "session"}, WantBy: "by drew (session)",
	}, { // Test 1: A named actor with no type recorded drops the empty parentheses.
		Decision: audit.DisclosedDecision{Actor: "drew"}, WantBy: "by drew",
	}, { // Test 2: No actor at all says so.
		Decision: audit.DisclosedDecision{}, WantBy: "with no actor recorded",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := decidedBy(test.Decision); got != test.WantBy {
				t.Errorf("decidedBy() = %q, want %q", got, test.WantBy)
			}
		})
	}
}
