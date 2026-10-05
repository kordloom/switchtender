package migration

import (
	"strings"
	"testing"
)

// adviceModule warns about the imported build template, the way a Conftest warning ported
// unchanged reads.
const adviceModule = `package switchtender

warn contains "the build template has no change ticket" if {
	input.run.playbook == "build.yml"
	not input.run.labels.ticket
}
`

// notedAdvice loads adviceModule with its warnings recorded rather than held.
const notedAdvice = `rego:
  - name: build-advice
    files: [rego/advice.rego]
    warn: note
`

// heldAdvice loads the same module at the default, so its warnings hold.
const heldAdvice = `rego:
  - name: build-advice
    files: [rego/advice.rego]
`

// TestARegoWarningNotedOnAnImportedTemplateIsOnTheRecord is the scenario for warn: note. An imported
// template launches under a Rego policy whose warning is a note: the run must execute with no
// approval, carry the note, and have a verified receipt whose outcome commits the note and whose
// rule set says the policy's warnings were noted. The file is then edited to the default, without a
// restart, and the same launch must wait for a person, held by the same warning, with no note and a
// rule set that no longer says so.
func TestARegoWarningNotedOnAnImportedTemplateIsOnTheRecord(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onPostgres, Policy: notedAdvice,
		Rego: map[string]string{"rego/advice.rego": adviceModule}})
	s := in.startServer("a")
	bundle := in.regoBundle(s, "build-advice")
	note := "build-advice (the build template has no change ticket, rego sha256:" + bundle[:12] + ")"
	rule := "build-advice: decided by Rego package data.switchtender, bundle sha256:" + bundle

	noted := in.launched(s, "operator", "build", nil)
	if got := in.waitStatus(s, noted.ID, "succeeded", "pending_approval", "failed"); got.Status !=
		"succeeded" {
		t.Fatalf("the noted launch reached %q, want it to run with no approval: %s", got.Status,
			describe(got.Raw))
	}
	if got := stringsOf(in.getRun(s, noted.ID).Raw["policy_notes"]); len(got) != 1 || got[0] != note {
		t.Errorf("the run's notes are %v, want [%s]", got, note)
	}

	ev := in.checkEvidence(s, noted.ID)
	rec := ev.Receipts[noted.ID]
	requireRecord(t, rec, recordWant{
		Launcher: "operator-laptop", OnBehalfOf: "operator", Playbook: "build.yml",
		Hosts: []string{"web1"}, Rules: []string{rule + ", warnings noted without holding"},
	})
	if got := stringsOf(rec.outcome(t).Outcome["policy_notes"]); len(got) != 1 || got[0] != note {
		t.Errorf("the verified outcome commits the notes %v, want [%s]", got, note)
	}

	in.writePolicy(heldAdvice, map[string]string{"rego/advice.rego": adviceModule})
	if again := in.regoBundle(s, "build-advice"); again != bundle {
		t.Fatalf("changing only the warn setting moved the bundle digest from %s to %s", bundle,
			again)
	}
	held := in.launched(s, "operator", "build", nil)
	waiting := in.waitStatus(s, held.ID, "pending_approval", "succeeded", "failed")
	if waiting.Status != "pending_approval" {
		t.Fatalf("under the default the launch reached %q, want it held for its warning",
			waiting.Status)
	}
	if by := str(waiting.Raw["held_by_policy"]); by != note {
		t.Errorf("the run is held by %q, want %q", by, note)
	}
	if got := stringsOf(waiting.Raw["policy_notes"]); len(got) != 0 {
		t.Errorf("a held warning was also recorded as a note: %v", got)
	}
	in.must(s, "approver", "POST", "/v1/runs/"+held.ID+"/approve", nil, 200)
	in.waitStatus(s, held.ID, "succeeded")

	ev = in.checkEvidence(s, held.ID)
	rec = ev.Receipts[held.ID]
	requireRecord(t, rec, recordWant{
		Launcher: "operator-laptop", OnBehalfOf: "operator", Approver: "approver-laptop",
		Playbook: "build.yml", Hosts: []string{"web1"}, Rules: []string{rule},
	})
	body := rec.outcome(t).OutcomeBody
	if strings.Contains(body, "policy_notes") || strings.Contains(body, "warnings noted") {
		t.Errorf("the held run's outcome still reads as noted: %s", body)
	}
}
