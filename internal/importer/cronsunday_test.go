package importer

import (
	"testing"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestCrontabSundayIsImported covers a weekly job that silently did not come across.
//
// Vixie cron accepts both 0 and 7 for Sunday, and a great many crontabs use 7. The parser this
// product schedules with caps the day-of-week field at 6 and refuses the line outright, so every
// Sunday entry was dropped during import with a warning that did not say a weekly backup had gone
// missing. The renumbering already existed for the Jenkins importer, which meets the same
// disagreement between dialects.
func TestCrontabSundayIsImported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which spelling of Sunday is being imported.
		Name string
		// In is the crontab expression.
		In string
		// Want is the expression after normalization.
		Want string
	}{{ // Test 0: The bare 7, the common spelling.
		Name: "bare seven", In: "0 3 * * 7", Want: "0 3 * * 0",
	}, { // Test 1: A range reaching Sunday.
		Name: "range through seven", In: "0 3 * * 5-7", Want: "0 3 * * 5,6,0",
	}, { // Test 2: A list containing Sunday.
		Name: "list with seven", In: "0 3 * * 1,7", Want: "0 3 * * 1,0",
	}, { // Test 3: Zero already means Sunday and is untouched.
		Name: "already zero", In: "0 3 * * 0", Want: "0 3 * * 0",
	}, { // Test 4: Weekdays are untouched.
		Name: "weekday range", In: "*/15 * * * 1-5", Want: "*/15 * * * 1-5",
	}, { // Test 5: A named schedule is not five fields and passes through.
		Name: "named schedule", In: "@daily", Want: "@daily",
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got := StandardizeCron(test.In)
			if got != test.Want {
				t.Errorf("%s: StandardizeCron(%q) = %q, want %q", test.Name, test.In, got, test.Want)
			}
			// The point of the rewrite is that the scheduler accepts the result. An expression the
			// parser refuses is a job that did not come across.
			if test.In != "@daily" {
				sc := &schedule.Schedule{Cron: got, Playbook: "site.yml"}
				if err := sc.Validate(); err != nil {
					t.Errorf("%s: the normalized expression %q is still refused: %v",
						test.Name, got, err)
				}
			}
		})
	}

	// The unnormalized form is what the scheduler actually refuses, which is why this matters.
	if err := (&schedule.Schedule{Cron: "0 3 * * 7", Playbook: "p"}).Validate(); err == nil {
		t.Log("the scheduler now accepts a bare 7 directly; the importer rewrite is belt and braces")
	}
}
