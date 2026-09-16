package server

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAChangeOutcomeIsDerivedNotDeclared covers the shape a single run cannot express.
//
// A rollback and a fix-forward is what a real change looks like: something failed, something else
// put it right, and the arc as a whole is neither a success nor a failure. Asked run by run, that
// reads as one red row and one green row with nothing joining them.
//
// The outcome is computed from the members rather than recorded by whoever closed it, so it cannot
// disagree with what the runs say happened.
func TestAChangeOutcomeIsDerivedNotDeclared(t *testing.T) {
	t.Parallel()
	at := func(d time.Duration) *time.Time {
		v := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Add(d)
		return &v
	}
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		Name        string
		Runs        []*run.Run
		WantOutcome string
		WantClosed  bool
	}{
		{
			Name: "rollback then fix forward",
			Runs: []*run.Run{
				{ID: "r1", Status: run.StatusFailed, CreatedAt: base, EndedAt: at(time.Minute), Actor: "ops"},
				{ID: "r2", Status: run.StatusSucceeded, CreatedAt: base.Add(time.Hour),
					EndedAt: at(2 * time.Hour), Actor: "ops"},
			},
			WantOutcome: changeMixed, WantClosed: true,
		},
		{
			Name: "still running",
			Runs: []*run.Run{
				{ID: "r1", Status: run.StatusSucceeded, CreatedAt: base, EndedAt: at(time.Minute)},
				{ID: "r2", Status: run.StatusRunning, CreatedAt: base.Add(time.Hour)},
			},
			// An unfinished member means the change is open, even though one member finished. A
			// close time here would say the change was done while work was still going.
			WantOutcome: changeInProgress, WantClosed: false,
		},
		{
			Name: "every member succeeded",
			Runs: []*run.Run{
				{ID: "r1", Status: run.StatusSucceeded, CreatedAt: base, EndedAt: at(time.Minute)},
			},
			WantOutcome: changeSucceeded, WantClosed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got := summarizeChange("deploy-the-fix", test.Runs)
			if got.Outcome != test.WantOutcome {
				t.Errorf("outcome = %q, want %q", got.Outcome, test.WantOutcome)
			}
			if closed := !got.ClosedAt.IsZero(); closed != test.WantClosed {
				t.Errorf("closed = %v, want %v", closed, test.WantClosed)
			}
			if got.OpenedAt != base {
				t.Errorf("opened at %v, want the earliest member's creation %v", got.OpenedAt, base)
			}
		})
	}
}
