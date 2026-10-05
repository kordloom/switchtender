package importer

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestAWorkflowOnlyExportIsAssessed pins that an export holding only workflows is read rather than
// refused as unrecognized. Every workflow in it is refused on its own terms, since a step inlines
// the job template it runs and none are in the export, and those refusals are the report: one of
// them names an approval gate the move would drop. Refusing the whole document as "nothing
// recognized" threw every reason away, and it named a list of kinds that did not include workflows.
func TestAWorkflowOnlyExportIsAssessed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the document holds.
		Name string
		// Export is the document.
		Export string
		// WantGates are the workflows the assessment must name as losing a gate.
		WantGates []string
		// WantLeftOut is a phrase one of the refusals must carry.
		WantLeftOut string
		// Want is the error expected.
		Want error
	}{{ // Test 0: A workflow holding only an approval node has no work for it to release.
		Name: "gated workflow alone",
		Export: `{"workflow_job_templates": [{"name": "Release", "workflow_nodes": [
			{"identifier": "approve", "related": {"create_approval_template": {"name": "Ship it"}}}]}]}`,
		WantLeftOut: "carries only approval nodes",
	}, { // Test 1: A workflow whose templates were not exported with it.
		Name: "workflow without its templates",
		Export: `{"workflow_job_templates": [{"name": "Nightly", "workflow_nodes": [
			{"identifier": "build", "unified_job_template": "Build"}]}]}`,
		WantLeftOut: "which is not a job template in this export",
	}, { // Test 2: A document that is not an AWX export is still refused as unrecognized.
		Name: "not an export", Export: `{"hosts": ["web01"]}`, Want: ErrNothingRecognized,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(test.Export), time.Now())
			if !errors.Is(err, test.Want) {
				t.Fatalf("FromAWX() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			a := plan.Assess()
			if diff := cmp.Diff(test.WantGates, a.Governance.ApprovalGates,
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ApprovalGates mismatch (-want +got):\n%s", diff)
			}
			says := func(w string) bool { return strings.Contains(w, test.WantLeftOut) }
			if !slices.ContainsFunc(a.Report.LeftOut, says) {
				t.Errorf("no refusal says %q: %v", test.WantLeftOut, a.Report.LeftOut)
			}
		})
	}
}

// TestTheReportDoesNotDenyTheGatesItNames pins the governance section of an export with approval
// gates and no templates. It named the gates the move drops and then said there was nothing here to
// gate, two sentences apart.
func TestTheReportDoesNotDenyTheGatesItNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the export holds.
		Name string
		// Governance is the assessment's governance section.
		Governance Governance
		// WantInDoc are phrases the report must carry.
		WantInDoc []string
		// WantNotInDoc are phrases it must not.
		WantNotInDoc []string
	}{{ // Test 0: Gates and no templates.
		Name:         "gates alone",
		Governance:   Governance{ApprovalGates: []string{"Release"}},
		WantInDoc:    []string{"1 workflow waits for a person", "no templates to grade"},
		WantNotInDoc: []string{"nothing here to gate"},
	}, { // Test 1: Neither, which really has nothing to gate.
		Name:      "neither",
		WantInDoc: []string{"nothing here to gate"},
	}, { // Test 2: A gate the move keeps is named as kept.
		Name:       "kept gate",
		Governance: Governance{Templates: 1, CarriedGates: []string{"Release"}},
		WantInDoc: []string{"1 workflow waits for a person at an approval node today. That gate " +
			"comes\n  across as an approval step", "      - Release\n"},
		WantNotInDoc: []string{"does not\n  come across"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var doc strings.Builder
			Render(&doc, "awx", "export.json", Assessment{Governance: test.Governance})
			for _, want := range test.WantInDoc {
				if !strings.Contains(doc.String(), want) {
					t.Errorf("the report does not say %q:\n%s", want, doc.String())
				}
			}
			for _, not := range test.WantNotInDoc {
				if strings.Contains(doc.String(), not) {
					t.Errorf("the report says %q:\n%s", not, doc.String())
				}
			}
		})
	}
}

// TestHeadlineOf pins the sentence the browser assessment leads with. It was written in the page's
// script from the counts, which made it a second implementation of the report's conclusion, and it
// disagreed with the first: one template read "None of your 1 templates", and an export whose
// workflows lose their approval gates led with a sentence that never mentioned them.
func TestHeadlineOf(t *testing.T) {
	t.Parallel()
	const gateOne = "1 workflow waits for a person at an approval node today, and that gate does " +
		"not come across."
	tests := []struct {
		// Name says what the assessment found.
		Name string
		// Governance is the assessment's governance section.
		Governance Governance
		// WantKind is gap or clear.
		WantKind string
		// WantResult is the sentence.
		WantResult string
	}{{ // Test 0: Nothing to grade and nothing gated.
		Name: "empty", WantKind: "clear",
		WantResult: "This export holds no templates, so there is nothing here to gate.",
	}, { // Test 1: One gated workflow and no templates.
		Name: "one gate", Governance: Governance{ApprovalGates: []string{"Release"}}, WantKind: "gap",
		WantResult: gateOne +
			" Rebuild it with an approval step before it runs, or it runs with no gate.",
	}, { // Test 2: Two gated workflows and no templates.
		Name: "two gates", Governance: Governance{ApprovalGates: []string{"A", "B"}}, WantKind: "gap",
		WantResult: "2 workflows wait for a person at an approval node today, and those gates do " +
			"not come across. Rebuild them with approval steps before they run, or they run with no gate.",
	}, { // Test 3: One template, irreversible.
		Name: "one irreversible", Governance: Governance{Templates: 1, WouldGate: 1}, WantKind: "gap",
		WantResult: "Your one template can do something nobody can undo, and today it runs whenever " +
			"somebody presses the button. One approval policy holds it until a second person agrees.",
	}, { // Test 4: One of several, irreversible.
		Name: "one of five", Governance: Governance{Templates: 5, WouldGate: 1}, WantKind: "gap",
		WantResult: "1 of your 5 templates can do something nobody can undo, and today it runs " +
			"whenever somebody presses the button. One approval policy holds it until a second " +
			"person agrees.",
	}, { // Test 5: Two of several, irreversible.
		Name: "two of five", Governance: Governance{Templates: 5, WouldGate: 2}, WantKind: "gap",
		WantResult: "2 of your 5 templates can do something nobody can undo, and today they run " +
			"whenever somebody presses the button. One approval policy holds them until a second " +
			"person agrees.",
	}, { // Test 6: One template, not irreversible.
		Name: "one reversible", Governance: Governance{Templates: 1}, WantKind: "clear",
		WantResult: "Your one template does not grade irreversible. A policy on risk or on tool is " +
			"the one to write here, not one on reversibility.",
	}, { // Test 7: Several templates, none irreversible.
		Name: "five reversible", Governance: Governance{Templates: 5}, WantKind: "clear",
		WantResult: "None of your 5 templates grades irreversible. A policy on risk or on tool is " +
			"the one to write here, not one on reversibility.",
	}, { // Test 8: Templates and a lost gate. The gate is said after the templates, never dropped.
		Name:       "templates and a gate",
		Governance: Governance{Templates: 5, WouldGate: 2, ApprovalGates: []string{"Release"}},
		WantKind:   "gap",
		WantResult: "2 of your 5 templates can do something nobody can undo, and today they run " +
			"whenever somebody presses the button. One approval policy holds them until a second " +
			"person agrees. " + gateOne,
	}, { // Test 9: Nothing irreversible, but a lost gate is still a gap.
		Name:       "reversible and a gate",
		Governance: Governance{Templates: 5, ApprovalGates: []string{"Release"}}, WantKind: "gap",
		WantResult: "None of your 5 templates grades irreversible. A policy on risk or on tool is " +
			"the one to write here, not one on reversibility. " + gateOne,
	}, { // Test 10: A gate the move keeps is said, and it is not a gap.
		Name:       "reversible and a kept gate",
		Governance: Governance{Templates: 5, CarriedGates: []string{"Release"}}, WantKind: "clear",
		WantResult: "None of your 5 templates grades irreversible. A policy on risk or on tool is " +
			"the one to write here, not one on reversibility. 1 workflow waits for a person at an " +
			"approval node, and that gate comes across as an approval step.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := HeadlineOf(Assessment{Governance: test.Governance})
			if diff := cmp.Diff(test.WantKind, got.Kind); diff != "" {
				t.Errorf("Kind mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantResult, got.Text); diff != "" {
				t.Errorf("Text mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
