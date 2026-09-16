package run

import (
	"fmt"
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
