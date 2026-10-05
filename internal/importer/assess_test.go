package importer

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// assessExport is an estate with the shape that makes an assessment worth reading: a routine
// template, two that destroy something, one credential two templates share, and one template
// targeting no inventory at all.
const assessExport = `{
 "projects":[{"name":"infra","scm_type":"git","scm_url":"https://example.invalid/i.git"}],
 "inventories":[{"name":"prod","hosts":[{"name":"db01"}]}],
 "credentials":[{"name":"prod-ssh","kind":"ssh"}],
 "job_templates":[
  {"name":"Deploy web","playbook":"site.yml","project":"infra","inventory":"prod","credentials":["prod-ssh"]},
  {"name":"Rotate logs","playbook":"logs.yml","project":"infra","inventory":"prod","credentials":["prod-ssh"]},
  {"name":"Decommission","playbook":"decom.yml","project":"infra","inventory":"prod",
   "extra_vars":"cmd: wipefs -a /dev/sdb"},
  {"name":"Drop schema","playbook":"db.yml","project":"infra","inventory":"prod",
   "extra_vars":"sql: DROP SCHEMA reporting CASCADE"},
  {"name":"Unscoped","playbook":"any.yml","project":"infra"}
 ]
}`

// TestAnAssessmentNamesWhatNobodyHasToApproveYet covers what this document exists to say.
//
// The counts are the product of the assessment; the sentence they support is "these templates can
// destroy something and today nobody has to agree first." A number that does not survive contact
// with what the product would actually do at run time makes that sentence worthless, which is why
// every grade here comes from the run-time graders rather than from a heuristic written for a
// sales document.
func TestAnAssessmentNamesWhatNobodyHasToApproveYet(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX([]byte(assessExport), time.Now())
	if err != nil {
		t.Fatalf("FromAWX: %v", err)
	}
	g := plan.Assess().Governance

	if g.Templates != 5 {
		t.Fatalf("graded %d templates, want 5", g.Templates)
	}

	// Test 0: The destructive pair is found, and found by the same grader a run would use. Both
	// hide their command in extra vars, which is where a destructive command actually rides.
	for _, want := range []string{"Decommission", "Drop schema"} {
		if !slices.Contains(g.Irreversible, want) {
			t.Errorf("template %q destroys data and was not graded irreversible: %v", want, g.Irreversible)
		}
		if !slices.Contains(g.HighRisk, want) {
			t.Errorf("template %q destroys data and was not graded high risk: %v", want, g.HighRisk)
		}
	}

	// Test 1: The routine ones are left alone. A section that flags everything is one a reader
	// learns to skip, and then it protects nothing.
	for _, safe := range []string{"Deploy web", "Rotate logs"} {
		if slices.Contains(g.Irreversible, safe) {
			t.Errorf("ordinary template %q graded irreversible, which makes the section noise", safe)
		}
	}

	// Test 2: The count a policy would hold matches what was named, or the headline disagrees with
	// the list under it.
	if g.WouldGate != len(g.Irreversible) {
		t.Errorf("policy would gate %d but %d templates were named", g.WouldGate, len(g.Irreversible))
	}

	// Test 3: A credential two templates share is where per-object access control stops meaning
	// anything, so it is named rather than left for somebody to notice.
	if len(g.SharedCredentials) != 1 || g.SharedCredentials[0].Templates != 2 {
		t.Errorf("shared credential not reported as used by 2 templates: %+v", g.SharedCredentials)
	}

	// Test 4: A template targeting no inventory decides what it reaches somewhere this cannot see.
	if !slices.Contains(g.NoInventory, "Unscoped") {
		t.Errorf("template with no inventory was not named: %v", g.NoInventory)
	}
}

// TestAnAssessmentSaysWhichGradesRestOnFilesItNeverRead is the honesty guard.
//
// An Ansible template keeps its work in a playbook, and at assessment time that playbook is in a
// repository nothing has fetched. Reading one can only raise a grade, so every number is a floor.
// Printing floors as though they were measurements is how a document earns a buyer's distrust the
// first time they find a destructive play that graded quiet.
func TestAnAssessmentSaysWhichGradesRestOnFilesItNeverRead(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX([]byte(assessExport), time.Now())
	if err != nil {
		t.Fatalf("FromAWX: %v", err)
	}
	if got := plan.Assess().Governance.Unread; got != 5 {
		t.Errorf("counted %d templates whose playbook went unread, want 5", got)
	}
}

// TestAnAssessmentCarriesTheSameReportAnImportWould keeps the two from drifting. A document that
// promised one thing and an import that did another is the failure the whole feature exists to
// prevent somebody finding in production.
func TestAnAssessmentCarriesTheSameReportAnImportWould(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX([]byte(assessExport), time.Now())
	if err != nil {
		t.Fatalf("FromAWX: %v", err)
	}
	a := plan.Assess()
	r := plan.Report()
	if a.Report.CreatedTotal != r.CreatedTotal || len(a.Report.LeftOut) != len(r.LeftOut) {
		t.Errorf("assessment report differs from the import report:\n assess %+v\n import %+v",
			a.Report, r)
	}
}

// TestAFleetImportIsAssessedWithoutClaimingToGateAnything covers Chef and Puppet, which bring no
// templates at all. The governance section has to say that plainly rather than printing a row of
// zeroes, which reads as an estate with nothing dangerous in it.
func TestAFleetImportIsAssessedWithoutClaimingToGateAnything(t *testing.T) {
	t.Parallel()
	plan, err := FromChef([]byte(chefExport), time.Now())
	if err != nil {
		t.Fatalf("FromChef: %v", err)
	}
	g := plan.Assess().Governance
	if g.Templates != 0 {
		t.Errorf("a fleet import graded %d templates, want 0", g.Templates)
	}
	if g.WouldGate != 0 {
		t.Errorf("a fleet import claims it would gate %d runs", g.WouldGate)
	}
}

// approvalExport is an awxkit export whose one workflow waits for a person between a build and a
// release. awxkit writes the approval node with create_approval_template where a job node carries
// its template reference.
const approvalExport = `{
 "projects":[{"name":"infra","scm_type":"git","scm_url":"https://example.invalid/i.git"}],
 "job_templates":[
  {"name":"Build","playbook":"build.yml","project":"infra"},
  {"name":"Ship","playbook":"ship.yml","project":"infra"}
 ],
 "workflow_job_templates":[{"name":"Release","related":{"workflow_nodes":[
  {"identifier":"build","unified_job_template":{"name":"Build"},
   "related":{"success_nodes":[{"identifier":"approve"}]}},
  {"identifier":"approve","related":{"success_nodes":[{"identifier":"ship"}],
   "create_approval_template":{"name":"Approve production","description":"","timeout":0}}},
  {"identifier":"ship","unified_job_template":{"name":"Ship"}}
 ]}}]
}`

// ungatedExport is the same estate with the build wired straight to the release.
const ungatedExport = `{
 "projects":[{"name":"infra","scm_type":"git","scm_url":"https://example.invalid/i.git"}],
 "job_templates":[
  {"name":"Build","playbook":"build.yml","project":"infra"},
  {"name":"Ship","playbook":"ship.yml","project":"infra"}
 ],
 "workflow_job_templates":[{"name":"Release","related":{"workflow_nodes":[
  {"identifier":"build","unified_job_template":{"name":"Build"},
   "related":{"success_nodes":[{"identifier":"ship"}]}},
  {"identifier":"ship","unified_job_template":{"name":"Ship"}}
 ]}}]
}`

// TestAnAssessmentNamesTheApprovalGatesAMoveDrops holds the document to the approvals an estate
// already has.
//
// An AWX approval node is the governance an estate carries today. It comes across as an approval
// step, and the document has to say the gate survives, or a reader who knows the estate is gated
// assumes the move drops it.
func TestAnAssessmentNamesTheApprovalGatesAMoveDrops(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Export    string
		WantGates []string
		// WantCarried are the workflows whose gates must be named as coming across.
		WantCarried  []string
		WantInDoc    []string
		WantNotInDoc []string
	}{{ // Test 0: The gate comes across as an approval step and the document says so.
		Name:        "workflow with an approval node",
		Export:      approvalExport,
		WantCarried: []string{"Release"},
		WantInDoc: []string{
			`workflow "Release" keeps its approval node, approve, as approval step`,
			"1 workflow waits for a person at an approval node today. That gate comes",
			"      - Release\n",
		},
		WantNotInDoc: []string{"create_approval_template", `runs ""`, "does not\n  come across"},
	}, { // Test 1: An estate with no approval node says nothing about one.
		Name:         "workflow without one",
		Export:       ungatedExport,
		WantNotInDoc: []string{"approval node", "approval gate"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(test.Export), time.Now())
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			a := plan.Assess()
			if diff := cmp.Diff(test.WantGates, a.Governance.ApprovalGates,
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ApprovalGates mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantCarried, a.Governance.CarriedGates,
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("CarriedGates mismatch (-want +got):\n%s", diff)
			}
			var doc strings.Builder
			Render(&doc, "awx", "export.json", a)
			for _, want := range test.WantInDoc {
				if !strings.Contains(doc.String(), want) {
					t.Errorf("the assessment does not say %q:\n%s", want, doc.String())
				}
			}
			for _, not := range test.WantNotInDoc {
				if strings.Contains(doc.String(), not) {
					t.Errorf("the assessment says %q:\n%s", not, doc.String())
				}
			}
		})
	}
}

// TestTheClosingParagraphCountsItsTemplates holds the assessment's closing sentences to the number
// they describe. An export holding one template printed "any of these 1 templates" and "1 of them
// run", in the paragraph a reader quotes, and the smallest export is the one a first visit to the
// browser assessment is most likely to try.
func TestTheClosingParagraphCountsItsTemplates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Governance   Governance
		WantInDoc    []string
		WantNotInDoc []string
	}{{ // Test 0: One template, unread and not irreversible.
		Name:       "one template",
		Governance: Governance{Templates: 1, Unread: 1},
		WantInDoc: []string{"and variables. It runs an Ansible playbook",
			"Today this template runs when somebody presses the button.",
			"It does not grade irreversible", "would not stop it."},
		WantNotInDoc: []string{"these 1 templates", "1 of them"},
	}, { // Test 1: One template that an irreversible hold would stop.
		Name:         "one irreversible template",
		Governance:   Governance{Templates: 1, WouldGate: 1, Irreversible: []string{"Wipe"}},
		WantInDoc:    []string{"would stop it until"},
		WantNotInDoc: []string{"stop 1 of", "these 1 templates"},
	}, { // Test 2: Several templates, one of them unread.
		Name:       "several templates, one unread",
		Governance: Governance{Templates: 3, Unread: 1},
		WantInDoc: []string{"and variables. 1 of them runs an Ansible playbook",
			"Today any of these 3 templates runs", "None of them grades irreversible"},
	}, { // Test 3: Several templates, several unread, some irreversible.
		Name:         "several templates, several unread",
		Governance:   Governance{Templates: 4, Unread: 2, WouldGate: 2},
		WantInDoc:    []string{"2 of them run an Ansible playbook", "would stop 2 of"},
		WantNotInDoc: []string{"Today this template"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var doc strings.Builder
			Render(&doc, "awx", "export.json", Assessment{Governance: test.Governance})
			text := strings.Join(strings.Fields(doc.String()), " ")
			for _, want := range test.WantInDoc {
				if !strings.Contains(text, want) {
					t.Errorf("the assessment does not say %q:\n%s", want, doc.String())
				}
			}
			for _, not := range test.WantNotInDoc {
				if strings.Contains(text, not) {
					t.Errorf("the assessment says %q:\n%s", not, doc.String())
				}
			}
		})
	}
}

// TestTheAssessmentListsEverythingThatDoesNotComeAcross pins the parts of the assessment a reader
// acts on. It capped what did not come across at six lines and "and N more", while the guide says that
// list is itemized and never summarized. It headed the counts of what the import would create "what is
// in there" and repeated their total as "comes across". It counted shared credentials and templates
// with no inventory without naming either.
func TestTheAssessmentListsEverythingThatDoesNotComeAcross(t *testing.T) {
	t.Parallel()
	var leftOut []string
	for i := 0; i < 9; i++ {
		leftOut = append(leftOut, fmt.Sprintf("dropped item %d was not imported", i))
	}
	a := Assessment{
		Report: Report{CreatedTotal: 3, Created: []Count{{Kind: "templates", N: 3}}, LeftOut: leftOut},
		Governance: Governance{
			Templates:         3,
			SharedCredentials: []CredentialUse{{ID: "cred_1", Name: "deploy-key", Templates: 2}},
			NoInventory:       []string{"Ad hoc cleanup"},
		},
	}
	var doc strings.Builder
	Render(&doc, "awx", "export.json", a)
	out := doc.String()
	for _, w := range leftOut {
		if !strings.Contains(out, w) {
			t.Errorf("the left out item %q is not listed:\n%s", w, out)
		}
	}
	for _, want := range []string{"What the import would create", "deploy-key, used by 2 templates",
		"- Ad hoc cleanup"} {
		if !strings.Contains(out, want) {
			t.Errorf("the assessment does not say %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"What is in there", "comes across   "} {
		if strings.Contains(out, gone) {
			t.Errorf("the assessment still says %q:\n%s", gone, out)
		}
	}
}

// TestAssessmentNoLongerLeavesRecurrencesOut pins the assessment's half of the recurrence change. A
// schedule cron cannot express used to be counted among what does not come across, which is the
// list a reader decides on; it now comes across and the assessment says so by counting it created
// and naming it nowhere among the losses.
func TestAssessmentNoLongerLeavesRecurrencesOut(t *testing.T) {
	t.Parallel()
	const export = `{"job_templates": [{"name": "close", "playbook": "close.yml",
	  "related": {"schedules": [
	    {"name": "quarter close", "rrule": "DTSTART;TZID=America/New_York:20260102T170000 ` +
		`RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR"},
	    {"name": "fortnightly", "rrule": "DTSTART:20260106T020000Z RRULE:FREQ=WEEKLY;INTERVAL=2"},
	    {"name": "but not at christmas", "rrule": "DTSTART:20260101T020000Z RRULE:FREQ=DAILY ` +
		`EXDATE:20261225T020000Z"}]}}]}`
	plan, err := FromAWX([]byte(export), importNow)
	if err != nil {
		t.Fatalf("FromAWX: %v", err)
	}
	report := plan.Assess().Report
	created := map[string]int{}
	for _, c := range report.Created {
		created[c.Kind] = c.N
	}
	if created["schedules"] != 3 {
		t.Errorf("schedules created = %d, want 3.\nleft out: %v", created["schedules"],
			report.LeftOut)
	}
	for _, line := range append(slices.Clone(report.LeftOut), report.NeedsReview...) {
		if strings.Contains(line, "schedule") {
			t.Errorf("a schedule is still listed: %s", line)
		}
	}
}
