package migration

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestDryRunThatForcesRealWorkIsHeldUnderYAMLAndRego is scenario twelve. AWX exports two check-mode
// templates: one whose playbook marks a task check_mode: false, so its dry run still writes, and
// one that changes nothing. A rule that exempts dry runs, written as a YAML exclude_dry_run rule or
// as the equivalent Rego module, has to hold the first and let the second through. The record of
// the held one has to say what the gate's scan read and found, the hold has to name the two clean
// fixes, and the record has to say who released it.
func TestDryRunThatForcesRealWorkIsHeldUnderYAMLAndRego(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the policy language.
		Name string
		// Policy is the policy file.
		Policy string
		// Rego are its modules.
		Rego map[string]string
		// WantRule is a fragment of the recorded rule that held the forcing dry run.
		WantRule string
		// WantFix is the hold note's second fix, worded for the policy language.
		WantFix string
	}{{
		Name: "yaml",
		Policy: `policies:
  - name: ansible-changes-wait
    tool: ansible
    exclude_dry_run: true
`,
		WantRule: "ansible-changes-wait: requires approval",
		WantFix:  `drop exclude_dry_run from "ansible-changes-wait"`,
	}, {
		Name:   "rego",
		Policy: "rego:\n  - name: ansible-changes-wait\n    files: [rego/changes.rego]\n",
		Rego: map[string]string{"rego/changes.rego": `package switchtender

hold contains "an Ansible change waits for a person" if {
	input.run.tool == "ansible"
	not input.run.dry_run
}
`},
		WantRule: "ansible-changes-wait: decided by Rego package data.switchtender, bundle sha256:",
		WantFix:  `stop exempting dry runs in the module of "ansible-changes-wait"`,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			in := newInstall(t, installOptions{Store: onSQLite, Policy: test.Policy, Rego: test.Rego})
			s := in.startServer("a")

			clean := in.launched(s, "operator", "preview clean", nil)
			done := in.waitDone(s, clean.ID)
			if done.Status != "succeeded" {
				t.Fatalf("the change-free dry run = %s, want it to run unheld: %s", done.Status,
					describe(done.Raw))
			}
			if got := in.marked("clean"); len(got) != 0 {
				t.Errorf("the change-free dry run wrote on %v, so it was not a dry run", got)
			}

			forced := in.launched(s, "operator", "preview forced", nil)
			held := in.waitStatus(s, forced.ID, "pending_approval")
			scans := str(held.Raw["dry_run_scans"])
			for _, want := range []string{`"scanner":"ansible-check-mode"`, `"forcing.yml"`,
				`forcing.yml: task \"Write a marker even under check mode\" sets check_mode to false`,
				`"classification":"not_change_free"`} {
				if !strings.Contains(scans, want) {
					t.Errorf("the held dry run's dry_run_scans = %s, want it to carry %s", scans, want)
				}
			}
			note := str(held.Raw["hold_note"])
			for _, want := range []string{"Two clean fixes", "rework the task so check mode is safe",
				test.WantFix} {
				if !strings.Contains(note, want) {
					t.Errorf("the held dry run's hold_note = %q, want it to say %q", note, want)
				}
			}
			if got := in.marked("forced"); len(got) != 0 {
				t.Fatalf("the held dry run wrote on %v before anyone approved it", got)
			}
			in.must(s, "approver", "POST", "/v1/runs/"+forced.ID+"/approve", nil, 200)
			in.waitStatus(s, forced.ID, "succeeded")
			if diff := cmp.Diff([]string{"web1"}, in.marked("forced")); diff != "" {
				t.Errorf("hosts the forcing dry run changed (-want +got):\n%s", diff)
			}

			ev := in.checkEvidence(s, clean.ID, forced.ID)
			requireRecord(t, ev.Receipts[clean.ID], recordWant{
				Launcher: "operator-laptop", OnBehalfOf: "operator", Playbook: "mark.yml",
				Hosts: []string{"web1"}, Rules: []string{test.WantRule},
			})
			rec := ev.Receipts[forced.ID]
			requireRecord(t, rec, recordWant{
				Launcher: "operator-laptop", OnBehalfOf: "operator", Approver: "approver-laptop",
				Playbook: "forcing.yml", Hosts: []string{"web1"}, Rules: []string{test.WantRule},
			})
			for _, id := range []string{clean.ID, forced.ID} {
				if spec := ev.Receipts[id].outcome(t).Spec; spec["dry_run"] != true {
					t.Errorf("the receipt of %s does not bind the run as a dry run: %v", id, spec)
				}
			}
		})
	}
}
