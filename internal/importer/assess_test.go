package importer

import (
	"slices"
	"testing"
	"time"
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
