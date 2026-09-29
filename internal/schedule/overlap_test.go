package schedule

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// countingSubmitter records how many runs a scheduler launched.
type countingSubmitter struct {
	// fired counts Submit calls.
	fired int
}

// Submit records a fire and returns a run with a predictable id.
func (c *countingSubmitter) Submit(_ context.Context, playbook, _ string,
	_ ...run.SubmitOption) (*run.Run, error) {
	c.fired++
	return &run.Run{ID: "run_fired", Playbook: playbook, Status: run.StatusPending}, nil
}

// SubmitSplit is unused by these tests.
func (c *countingSubmitter) SubmitSplit(ctx context.Context, playbook, inv string, _ int,
	opts ...run.SubmitOption) (*run.Run, error) {
	return c.Submit(ctx, playbook, inv, opts...)
}

// SubmitPipeline is unused by these tests.
func (c *countingSubmitter) SubmitPipeline(ctx context.Context, name, inv string,
	_ []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	return c.Submit(ctx, name, inv, opts...)
}

// TestScheduleWaitsForItsOwnPreviousRun covers work stacking on top of itself.
//
// A schedule fired whenever its next run time came due, with no regard for what it started last
// time. A five-minute schedule whose playbook takes eight minutes accumulated concurrent copies of
// itself against the same hosts, and tasks that are not idempotent interleaved: two runs installing
// a package, restarting a service, or holding one lock, neither aware of the other. Nothing capped
// how many piled up.
func TestScheduleWaitsForItsOwnPreviousRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	due := time.Now().Add(-time.Minute)

	sc := &Schedule{
		ID: "sch_1", Name: "nightly", Cron: "* * * * *", Enabled: true,
		NextRunAt: &due, LastRunID: "run_previous",
	}
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The previous run is still going.
	active := "run_previous"
	sub := &countingSubmitter{}
	s := NewScheduler(store, sub, zap.NewNop(),
		WithRunActive(func(context.Context, string, string) (string, error) { return active, nil }))
	s.ctx = ctx

	s.tick(time.Now())
	if sub.fired != 0 {
		t.Errorf("the schedule fired %d times while its previous run was still going", sub.fired)
	}

	// The tick still advanced the schedule, so it stays on its cadence rather than firing the
	// instant the slow run ends.
	after, err := store.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.NextRunAt == nil || !after.NextRunAt.After(due) {
		t.Errorf("next run time did not advance past the skipped slot: %v", after.NextRunAt)
	}

	// Once the previous run finishes, the schedule fires again.
	active = ""
	past := time.Now().Add(-time.Minute)
	after.NextRunAt = &past
	if err := store.Save(ctx, after); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.tick(time.Now())
	if sub.fired != 1 {
		t.Errorf("the schedule fired %d times after its previous run finished, want 1", sub.fired)
	}
}

// TestScheduleWithNoOverlapCheckStillFires confirms the option is what turns this on, so a caller
// that wants the old overlapping behavior keeps it.
func TestScheduleWithNoOverlapCheckStillFires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	due := time.Now().Add(-time.Minute)
	if err := store.Save(ctx, &Schedule{
		ID: "sch_2", Name: "overlapping", Cron: "* * * * *", Enabled: true,
		NextRunAt: &due, LastRunID: "run_previous",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sub := &countingSubmitter{}
	s := NewScheduler(store, sub, zap.NewNop())
	s.ctx = ctx
	s.tick(time.Now())

	if sub.fired != 1 {
		t.Errorf("fired %d times without the option, want 1", sub.fired)
	}
}

// TestActiveInSeesEveryRunTheScheduleFired pins what counts as a schedule's work still going. The
// check read only the run the schedule last created, and a scheduled terraform apply under a
// plan-content rule is planned first: the plan run finishes the moment it proposes the apply, so
// the next tick found the schedule's last run done and fired another plan while the first apply was
// still held for a person or running. Each tick added a held apply, and approving the queue ran
// them one after another against the same state. The proposed apply carries the plan's source, so
// any unfinished run fired under the schedule's name keeps the next fire waiting.
func TestActiveInSeesEveryRunTheScheduleFired(t *testing.T) {
	t.Parallel()

	fromSchedule := func(id string, status run.Status, schedule string) *run.Run {
		return &run.Run{ID: id, Tool: run.ToolTerraform, Command: "infra/prod", Status: status,
			Source: "schedule", SourceID: schedule, CreatedAt: time.Now()}
	}
	tests := []struct {
		// Name says what the schedule left behind.
		Name string
		// Runs are the runs in the store.
		Runs []*run.Run
		// LastRunID is the run the schedule last created.
		LastRunID string
		// WantResult is the run the schedule is still waiting on, empty when it waits on none.
		WantResult string
	}{{ // Test 0: The run it last created is still going.
		Name:      "last run running",
		Runs:      []*run.Run{fromSchedule("run_last", run.StatusRunning, "sch_1")},
		LastRunID: "run_last", WantResult: "run_last",
	}, { // Test 1: The plan finished and the apply it proposed is held for a person.
		Name: "plan done, apply held",
		Runs: []*run.Run{fromSchedule("run_last", run.StatusSucceeded, "sch_1"),
			fromSchedule("run_apply", run.StatusPendingApproval, "sch_1")},
		LastRunID: "run_last", WantResult: "run_apply",
	}, { // Test 2: The plan finished and the apply it proposed is queued.
		Name: "plan done, apply queued",
		Runs: []*run.Run{fromSchedule("run_last", run.StatusSucceeded, "sch_1"),
			fromSchedule("run_apply", run.StatusPending, "sch_1")},
		LastRunID: "run_last", WantResult: "run_apply",
	}, { // Test 3: The plan finished and the apply it proposed is running.
		Name: "plan done, apply running",
		Runs: []*run.Run{fromSchedule("run_last", run.StatusSucceeded, "sch_1"),
			fromSchedule("run_apply", run.StatusRunning, "sch_1")},
		LastRunID: "run_last", WantResult: "run_apply",
	}, { // Test 4: Another schedule's held run is not this schedule's work.
		Name: "another schedule's hold",
		Runs: []*run.Run{fromSchedule("run_last", run.StatusSucceeded, "sch_1"),
			fromSchedule("run_other", run.StatusPendingApproval, "sch_2")},
		LastRunID: "run_last",
	}, { // Test 5: Everything the schedule fired has finished.
		Name: "all finished",
		Runs: []*run.Run{fromSchedule("run_last", run.StatusSucceeded, "sch_1"),
			fromSchedule("run_apply", run.StatusFailed, "sch_1")},
		LastRunID: "run_last",
	}, { // Test 6: The last run was pruned, and a run the schedule fired is still going.
		Name:      "last run pruned, another running",
		Runs:      []*run.Run{fromSchedule("run_apply", run.StatusRunning, "sch_1")},
		LastRunID: "run_pruned", WantResult: "run_apply",
	}, { // Test 7: The schedule never fired.
		Name: "never fired",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			runs := run.NewMemStore()
			for _, r := range test.Runs {
				if err := runs.Save(ctx, r); err != nil {
					t.Fatalf("Save(%s) error = %v", r.ID, err)
				}
			}
			got, err := ActiveIn(runs)(ctx, "sch_1", test.LastRunID)
			if err != nil {
				t.Fatalf("ActiveIn() error = %v", err)
			}
			if got != test.WantResult {
				t.Errorf("ActiveIn() = %q, want %q", got, test.WantResult)
			}
		})
	}
}

// TestAScheduledPlanDoesNotStackHeldApplies runs the tick itself over a store holding a scheduled
// plan that finished and the apply it proposed, still held. The schedule must skip its fire rather
// than plan again and queue a second apply behind the first.
func TestAScheduledPlanDoesNotStackHeldApplies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	schedules := NewMemStore()
	due := time.Now().Add(-time.Minute)
	if err := schedules.Save(ctx, &Schedule{
		ID: "sch_1", Name: "nightly apply", Cron: "* * * * *", Enabled: true,
		NextRunAt: &due, LastRunID: "run_plan",
	}); err != nil {
		t.Fatalf("Save(schedule) error = %v", err)
	}
	runs := run.NewMemStore()
	for _, r := range []*run.Run{
		{ID: "run_plan", Tool: run.ToolTerraform, Status: run.StatusSucceeded, Source: "schedule",
			SourceID: "sch_1", CreatedAt: time.Now()},
		{ID: "run_apply", Tool: run.ToolTerraform, Status: run.StatusPendingApproval,
			Source: "schedule", SourceID: "sch_1", ProposedFrom: "run_plan", CreatedAt: time.Now()},
	} {
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	sub := &countingSubmitter{}
	s := NewScheduler(schedules, sub, zap.NewNop(), WithRunActive(ActiveIn(runs)))
	s.ctx = ctx

	s.tick(time.Now())
	if sub.fired != 0 {
		t.Errorf("the schedule fired %d times while the apply its last plan proposed was held",
			sub.fired)
	}
}
