package outcome

import (
	"context"
	"fmt"
	"testing"

	"github.com/kordloom/loomseal/jcs"

	"github.com/kordloom/switchtender/internal/run"
)

// TestBodyTaskDurationsAreJCSSafe proves the outcome record of a real run, whose task durations are
// fractional seconds, reduces to bytes the LoomSeal JCS profile accepts. The profile refuses
// non-integer numbers, so a float anywhere in the record makes every range receipt unsignable.
func TestBodyTaskDurationsAreJCSSafe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Seconds float64
		WantMS  int64
	}{{ // Test 0: The fractional duration every real run produces.
		Seconds: 0.013500213, WantMS: 14,
	}, { // Test 1: A whole-second duration.
		Seconds: 3, WantMS: 3000,
	}, { // Test 2: A zero duration.
		Seconds: 0, WantMS: 0,
	}, { // Test 3: A half-millisecond rounds rather than truncates.
		Seconds: 0.0015, WantMS: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			// Mirror the real order: summaries land while the run is live, then it finalizes.
			r := &run.Run{ID: "run_jcs", Playbook: "site.yml", Status: run.StatusRunning}
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save(running) error = %v", err)
			}
			if err := store.SaveTaskSummary(ctx, r.ID, []run.TaskSummary{
				{Task: "deploy", Seconds: test.Seconds},
			}); err != nil {
				t.Fatalf("SaveTaskSummary() error = %v", err)
			}
			r.Status = run.StatusSucceeded
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save(succeeded) error = %v", err)
			}

			body, err := Body(ctx, store, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			if _, err := jcs.Canonicalize(body); err != nil {
				t.Fatalf("Canonicalize() refused the outcome body: %v", err)
			}
			rec, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(rec.Tasks) != 1 || rec.Tasks[0].Milliseconds != test.WantMS {
				t.Errorf("Tasks = %+v, want one task at %d ms", rec.Tasks, test.WantMS)
			}
		})
	}
}
