package run

import (
	"fmt"
	"strings"
	"testing"
)

// TestReversibilityGradesWhatCannotBeTakenBack covers the distinction the class exists to draw.
//
// Risk already grades how bad an outcome is. It cannot express the question an approver actually
// needs answered before releasing a held run, which is whether they get a second chance. A fleet
// restart is high risk and undoes itself. Deleting a table grades quietly and is permanent. A rule
// written on risk catches the restart and misses the delete.
func TestReversibilityGradesWhatCannotBeTakenBack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Command   string
		WantClass string
		DryRun    bool
	}{
		{Command: "terraform destroy -auto-approve", WantClass: Irreversible}, // Test 0.
		{Command: "rm -rf /var/lib/data", WantClass: Irreversible},            // Test 1.
		{Command: "psql -c 'DROP TABLE orders'", WantClass: Irreversible},     // Test 2: case folded.
		{Command: "mkfs.ext4 /dev/sdb1", WantClass: Irreversible},             // Test 3.
		// Test 4: destructive and fully reversible. The machine comes back. Grading this the same
		// as a dropped table is what makes an irreversibility rule fire constantly and get removed.
		{Command: "shutdown -r now", WantClass: ReversibleCostly},
		// Test 5: the honest default. It changed something, and undoing it means running something.
		{Command: "systemctl restart nginx", WantClass: ReversibleCostly},
		// Test 6: a dry run is the only thing with nothing to undo, even when it would destroy.
		{Command: "terraform destroy", DryRun: true, WantClass: Reversible},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := AssessReversibility(&Run{Command: test.Command, DryRun: test.DryRun})
			if got.Class != test.WantClass {
				t.Errorf("AssessReversibility(%q) = %q, want %q", test.Command, got.Class, test.WantClass)
			}
			if len(got.Reasons) == 0 {
				t.Errorf("no reason given, so an approver sees a class with nothing behind it")
			}
		})
	}
	if got := AssessReversibility(nil).Class; got != Reversible {
		t.Errorf("a nil run graded %q, want %q", got, Reversible)
	}
}

// TestAReversibilityFloorCoversEverythingHarderToUndo covers the floor's direction, and the typo
// case that would otherwise disable a rule without saying so.
func TestAReversibilityFloorCoversEverythingHarderToUndo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Class string
		Floor string
		Want  bool
	}{
		{Class: Irreversible, Floor: ReversibleCostly, Want: true},     // Test 0: floors look upward.
		{Class: ReversibleCostly, Floor: ReversibleCostly, Want: true}, // Test 1: inclusive.
		{Class: Reversible, Floor: ReversibleCostly, Want: false},      // Test 2: a dry run is left alone.
		{Class: Irreversible, Floor: Irreversible, Want: true},         // Test 3: the narrowest rule.
		{Class: ReversibleCostly, Floor: Irreversible, Want: false},    // Test 4.
		// Test 5: a misspelled floor matches nothing rather than everything. Validate refuses it at
		// load so a policy file cannot reach here, and if one ever does it fails closed on scope.
		{Class: Irreversible, Floor: "irreversable", Want: false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := MeetsReversibilityFloor(test.Class, test.Floor); got != test.Want {
				t.Errorf("MeetsReversibilityFloor(%q, %q) = %v, want %v",
					test.Class, test.Floor, got, test.Want)
			}
		})
	}
}

// TestAPlaybookGradeSaysWhatItLookedAt covers the limit an approver has to be told about.
//
// Reversibility reads the command. An Ansible run carries a playbook path and the destructive work
// is inside a file this never opens, so a playbook that drops every database grades the same as one
// that restarts a service. That is the flagship tool, so the blindness sits exactly where the
// product is used most.
//
// The class stays costly rather than climbing on a guess, because inferring from a file name would
// put false confidence behind the word irreversible. The reason carries the limit instead. If the
// class is ever made to climb, this test should be the thing that stops it being done by guessing.
func TestAPlaybookGradeSaysWhatItLookedAt(t *testing.T) {
	t.Parallel()
	got := AssessReversibility(&Run{Tool: ToolAnsible, Playbook: "/srv/wipe-all-databases.yml"})
	if got.Class != ReversibleCostly {
		t.Errorf("class = %q, want %q", got.Class, ReversibleCostly)
	}
	var said bool
	for _, reason := range got.Reasons {
		if strings.Contains(reason, "were not examined") {
			said = true
		}
	}
	if !said {
		t.Errorf("the grade does not say the playbook was not read, so an approver cannot tell "+
			"the grade is blind to what the playbook does: %v", got.Reasons)
	}
}

// TestNothingIsIrreversibleAndLowRisk holds the one relationship between the two graders.
//
// They answer different questions on purpose: risk asks how bad it is if this goes wrong, and
// reversibility asks whether you get a second chance. Those come apart in one direction only. A
// reboot is high risk and fully reversible, which is fine and is why the lists differ. The other
// direction is not fine: a command that cannot be undone has already gone as wrong as it can, so
// grading it low risk tells an approver to relax about the one run they cannot take back.
//
// This drifted in exactly that direction twice. "drop schema" reached the risk list and never
// reached the permanence list. "rm -r -f" was read as flags by one grader and matched as a string
// by the other, so the same command was irreversible and low risk at once.
func TestNothingIsIrreversibleAndLowRisk(t *testing.T) {
	t.Parallel()
	commands := []string{
		"aws s3 rb s3://prod-backups --force", "kubectl delete namespace production",
		"helm uninstall payments -n prod", "wipefs -a /dev/sdb", "shred -u /etc/keys/master.pem",
		"psql -c \"DROP SCHEMA public CASCADE\"", "find /var/backups -mtime +30 -delete",
		"rm -r -f /var/lib/data", "rm -f -r /var/lib/data", "rm --recursive --force /data",
		"rm -rf /tmp/scratch", "tofu apply -destroy", "terraform apply -destroy tfplan",
		"az group delete --name prod-rg --yes", "gcloud compute instances delete web-1 --quiet",
		"aws rds delete-db-instance --db-instance-identifier prod",
	}
	for _, c := range commands {
		r := &Run{Tool: ToolBash, Command: c}
		rev := AssessReversibility(r)
		risk := AssessRisk(r)
		if rev.Class != Irreversible {
			t.Errorf("%q graded %q, want irreversible", c, rev.Class)
			continue
		}
		if risk.Level != RiskHigh {
			t.Errorf("%q cannot be undone yet graded %q risk, so an approver is told to relax "+
				"about the one run they cannot take back", c, risk.Level)
		}
	}
}

// TestAnOrdinaryCommandIsNotGradedPermanent keeps the markers from widening until the grade means
// nothing. A rule that fires on everything gets switched off, and then it protects nothing.
func TestAnOrdinaryCommandIsNotGradedPermanent(t *testing.T) {
	t.Parallel()
	for _, c := range []string{
		"systemctl restart nginx", "ansible-playbook site.yml", "aws s3 ls",
		"rm -i /tmp/one.txt", "kubectl get pods", "terraform plan", "apt-get install -y curl",
		"kubectl delete deployment web", "helm upgrade payments ./chart",
	} {
		if got := AssessReversibility(&Run{Tool: ToolBash, Command: c}).Class; got == Irreversible {
			t.Errorf("%q graded irreversible, which widens the rule until it is ignored", c)
		}
	}
}
