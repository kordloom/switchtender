package schedule

import (
	"context"
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
	active := true
	sub := &countingSubmitter{}
	s := NewScheduler(store, sub, zap.NewNop(),
		WithRunActive(func(context.Context, string) (bool, error) { return active, nil }))
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
	active = false
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
