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

// TestReversibilityReadsEachCommandInALine pins the command judgments both ways. A destroy is still
// a destroy when a deploy script writes it behind cd and &&, and in every spelling Terraform and
// OpenTofu accept. The benign neighbors stay benign: find without -delete, rsync without --delete,
// del without a force flag, and a plan that previews a destroy. Graded permanent, those would hold
// ordinary runs under an irreversibility rule until somebody removed the rule.
func TestReversibilityReadsEachCommandInALine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Command   string
		WantClass string
	}{
		// Test 0: Behind cd.
		{Command: "cd infra && terraform apply -destroy -auto-approve", WantClass: Irreversible},
		// Test 1: Assigned.
		{Command: "tofu apply -destroy=true", WantClass: Irreversible},
		// Test 2: Doubled dash.
		{Command: "terraform apply --destroy", WantClass: Irreversible},
		// Test 3: A preview.
		{Command: "terraform plan -destroy", WantClass: ReversibleCostly},
		// Test 4: Behind cd.
		{Command: "cd /srv/cache && find . -name '*.tmp' -delete", WantClass: Irreversible},
		// Test 5: No -delete.
		{Command: "find /var/log -name '*.gz' -print", WantClass: ReversibleCostly},
		// Test 6: Prefixed.
		{Command: "rsync -a --delete-after /src/ /dst/", WantClass: Irreversible},
		// Test 7: No --delete.
		{Command: "rsync -a /src/ /dst/", WantClass: ReversibleCostly},
		// Test 8: Quiet.
		{Command: `del /q C:\temp\build.log`, WantClass: Irreversible},
		// Test 9: Forced.
		{Command: `del /f C:\temp\build.log`, WantClass: Irreversible},
		// Test 10: It asks.
		{Command: `del C:\temp\build.log`, WantClass: ReversibleCostly},
		// Test 11: Two commands on one line do not lend each other their tokens.
		{Command: "find /var/log -name '*.gz' -print; echo -delete", WantClass: ReversibleCostly},
		// Test 12: A quiet copy is not a forced delete.
		{Command: `xcopy /q /e C:\src C:\dst`, WantClass: ReversibleCostly},
		// Test 13: A path holding the letters r and f is not a flag.
		{Command: "rm /srv/reports/draft.txt", WantClass: ReversibleCostly},
		// Test 14: Recursive without force, since nothing in a run is there to be prompted.
		{Command: "rm -r /var/lib/data", WantClass: Irreversible},
		// Test 15: The capital spelling of the same flag.
		{Command: "rm -R /srv/cache", WantClass: Irreversible},
		// Test 16: The long spelling of the same flag.
		{Command: "sudo rm --recursive /srv/cache", WantClass: Irreversible},
		// Test 17: Force alone removes one file, which a floor rule would hold on every cleanup.
		{Command: "rm -f /tmp/build.log", WantClass: ReversibleCostly},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := AssessReversibility(&Run{Command: test.Command}); got.Class != test.WantClass {
				t.Errorf("AssessReversibility(%q) = %q (%v), want %q", test.Command, got.Class,
					got.Reasons, test.WantClass)
			}
		})
	}
}

// TestReversibilityWeighsWhatTheRunLeftBehind pins how evidence moves a grade. Only a finished run
// whose hosts all reported no change is cleared as having nothing to undo; a finished run with no
// host results at all proves nothing, and neither does a run still in flight. A destructive command
// passed as an extra var and a playbook's own permanent task each grade the run irreversible, and a
// playbook read with nothing permanent says when includes went unread.
func TestReversibilityWeighsWhatTheRunLeftBehind(t *testing.T) {
	t.Parallel()
	dropDB := []byte(`- hosts: db
  tasks:
    - name: drop the orders database
      community.postgresql.postgresql_db:
        name: orders
        state: absent
`)
	restartAndInclude := []byte(`- hosts: web
  tasks:
    - name: restart nginx
      ansible.builtin.service:
        name: nginx
        state: restarted
    - ansible.builtin.include_role:
        name: hardening
`)
	quiet := []HostSummary{{Host: "web01"}, {Host: "web02"}}
	tests := []struct {
		Name       string
		Run        Run
		Evidence   ReversibilityEvidence
		WantClass  string
		WantReason string
	}{{ // Test 0: A finished destructive run with no host results is not cleared by their absence.
		Name:      "finished without host results",
		Run:       Run{Tool: ToolBash, Command: "rm -rf /var/lib/data", Status: StatusSucceeded},
		WantClass: Irreversible,
	}, { // Test 1: A finished run whose every host reported no change has nothing to undo.
		Name:      "finished with no change",
		Run:       Run{Tool: ToolAnsible, Playbook: "site.yml", Status: StatusSucceeded},
		Evidence:  ReversibilityEvidence{Hosts: quiet},
		WantClass: Reversible,
	}, { // Test 2: A run still in flight has changed nothing yet, which is not the same as nothing.
		Name:      "still running",
		Run:       Run{Tool: ToolAnsible, Playbook: "site.yml", Status: StatusRunning},
		Evidence:  ReversibilityEvidence{Hosts: quiet},
		WantClass: ReversibleCostly,
	}, { // Test 3: A destructive command riding in an extra var is graded like one on the line.
		Name: "destroy in an extra var",
		Run: Run{Tool: ToolAnsible, Playbook: "cleanup.yml",
			ExtraVars: map[string]any{"cleanup_cmd": "rm -rf /srv/data"}},
		WantClass: Irreversible,
	}, { // Test 4: The playbook's own text holds a permanent task, whatever its file is called.
		Name:       "permanent task in the playbook",
		Run:        Run{Tool: ToolAnsible, Playbook: "maintenance.yml"},
		Evidence:   ReversibilityEvidence{Playbook: dropDB},
		WantClass:  Irreversible,
		WantReason: "postgresql_db with state absent",
	}, { // Test 5: A playbook read with nothing permanent says the includes it did not follow.
		Name:       "include not followed",
		Run:        Run{Tool: ToolAnsible, Playbook: "web.yml"},
		Evidence:   ReversibilityEvidence{Playbook: restartAndInclude},
		WantClass:  ReversibleCostly,
		WantReason: "roles and includes were not followed",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := test.Run
			got := AssessReversibilityFrom(&r, test.Evidence)
			if got.Class != test.WantClass {
				t.Errorf("class = %q (%v), want %q", got.Class, got.Reasons, test.WantClass)
			}
			said := strings.Join(got.Reasons, "\n")
			if test.WantReason != "" && !strings.Contains(said, test.WantReason) {
				t.Errorf("reasons %v do not say %q", got.Reasons, test.WantReason)
			}
		})
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
		// Test 6: the widest floor covers every run, the fully reversible included.
		{Class: Reversible, Floor: Reversible, Want: true},
		// Test 7: and every class harder to undo than it.
		{Class: ReversibleCostly, Floor: Reversible, Want: true},
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
	for _, class := range []string{Reversible, ReversibleCostly, Irreversible} {
		if !ValidReversibility(class) {
			t.Errorf("ValidReversibility(%q) = false, so a policy naming it would be refused", class)
		}
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
		// The spellings an adversarial review proved dodged the regex this used to be: a path
		// prefix, a privilege or applet wrapper, flags after the operand, and a sequence operator
		// in front. Each executed as root-level destruction and graded fully reversible.
		"/bin/rm -rf /var/lib/data", "sudo rm -rf /var/lib/data", "busybox rm -rf /data",
		"rm /var/lib/data -rf", "cd /srv && rm -rf ./releases",
		"rsync -a --delete /empty/ /var/backups/",
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
		// The false positives the same review found: a read-only destroy preview, tokens that
		// merely contain a marker's letters, and paths sitting safely after rm's -- terminator.
		"terraform plan -destroy", "pip install pre-delete-hook",
		"rm -- -rf", "informat -rf /data", "cp model /files/latest",
	} {
		if got := AssessReversibility(&Run{Tool: ToolBash, Command: c}).Class; got == Irreversible {
			t.Errorf("%q graded irreversible, which widens the rule until it is ignored", c)
		}
	}
}

// TestAPlannedDestroyIsGradedFromThePlan pins the grade of an apply a plan proposed. The command of
// a terraform or opentofu run names only a working directory, so the grade had nothing to go on and
// called every apply costly and medium risk, including one whose plan said it would destroy three
// resources. A rule holding irreversible changes for a second person never saw it. The plan's
// destroy count is the evidence, and any destroy is permanent from here.
func TestAPlannedDestroyIsGradedFromThePlan(t *testing.T) {
	t.Parallel()
	count := func(n int) *int { return &n }
	tests := []struct {
		// Name says what the plan found.
		Name string
		// Destroys is the plan's destroy count, nil for no plan read.
		Destroys *int
		// DryRun marks a run that changes nothing.
		DryRun bool
		// WantClass is the reversibility class.
		WantClass string
		// WantRisk is the risk level.
		WantRisk string
		// WantReason is a phrase the reversibility reasons must carry, empty for none checked.
		WantReason string
	}{{ // Test 0: A plan that destroys three resources is permanent and high risk.
		Name: "destroys three", Destroys: count(3), WantClass: Irreversible, WantRisk: RiskHigh,
		WantReason: "its plan destroys 3 resources",
	}, { // Test 1: One destroy is enough, and is said in the singular.
		Name: "destroys one", Destroys: count(1), WantClass: Irreversible, WantRisk: RiskHigh,
		WantReason: "its plan destroys 1 resource,",
	}, { // Test 2: A plan that destroys nothing leaves the apply where any apply stands.
		Name: "destroys none", Destroys: count(0), WantClass: ReversibleCostly, WantRisk: RiskMedium,
	}, { // Test 3: With no plan read there is no evidence either way.
		Name: "no plan read", WantClass: ReversibleCostly, WantRisk: RiskMedium,
	}, { // Test 4: A dry run changes nothing whatever a plan says.
		Name: "dry run", Destroys: count(3), DryRun: true, WantClass: Reversible, WantRisk: RiskLow,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := &Run{Tool: ToolTerraform, Command: "infra/prod", DryRun: test.DryRun,
				PlanDestroys: test.Destroys}
			undo := AssessReversibility(r)
			if undo.Class != test.WantClass {
				t.Errorf("class = %q, want %q (reasons %v)", undo.Class, test.WantClass, undo.Reasons)
			}
			reasons := strings.Join(undo.Reasons, "; ")
			if test.WantReason != "" && !strings.Contains(reasons, test.WantReason) {
				t.Errorf("reasons %v do not say %q", undo.Reasons, test.WantReason)
			}
			if risk := AssessRisk(r); risk.Level != test.WantRisk {
				t.Errorf("risk = %q, want %q (reasons %v)", risk.Level, test.WantRisk, risk.Reasons)
			}
		})
	}
}
