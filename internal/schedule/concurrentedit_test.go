package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// editingSubmitter fires like a normal submitter and runs a hook while the run is in flight, which
// is where an operator's concurrent edit or delete lands.
type editingSubmitter struct {
	// runID is returned as the created run's id.
	runID string
	// during runs once inside Submit, standing in for an operator acting mid-fire.
	during func()
	// calls counts fires.
	calls int
}

// Submit records the call, runs the hook, and returns a run.
func (e *editingSubmitter) Submit(context.Context, string, string,
	...run.SubmitOption) (*run.Run, error) {
	e.calls++
	if e.during != nil {
		e.during()
		e.during = nil
	}
	return &run.Run{ID: e.runID}, nil
}

// SubmitSplit is unused here and reports a run without firing anything.
func (e *editingSubmitter) SubmitSplit(context.Context, string, string, int,
	...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: e.runID}, nil
}

// SubmitPipeline is unused here and reports a run without firing anything.
func (e *editingSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep,
	...run.SubmitOption) (*run.Run, error) {
	return &run.Run{ID: e.runID}, nil
}

// seedDue saves one enabled schedule already due, and returns the store.
func seedDue(t *testing.T, store Store) {
	t.Helper()
	past := time.Now().Add(-time.Minute)
	if err := store.Save(context.Background(), &Schedule{
		ID: "s1", Cron: "* * * * *", Playbook: "p.yml", Enabled: true,
		CreatedAt: time.Now(), NextRunAt: &past,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// TestSchedulerDeletedDuringFireStopsFiring is the case that matters most: deleting a schedule has
// to stop it.
//
// The tick took a snapshot of every schedule, fired, then wrote the whole snapshot back. A delete
// landing while the run was in flight was undone by that write, and because the row came back
// enabled with a live next run time, the schedule kept firing on every later tick. An operator
// deletes a schedule precisely because they want it stopped, usually because it is doing something
// wrong, so this is the version of the bug with no upper bound on the damage.
func TestSchedulerDeletedDuringFireStopsFiring(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	seedDue(t, store)

	sub := &editingSubmitter{runID: "run_x", during: func() {
		if err := store.Delete(ctx, "s1"); err != nil {
			t.Errorf("Delete() during fire error = %v", err)
		}
	}}
	s := NewScheduler(store, sub, nil)

	s.tick(time.Now())
	// A second tick two minutes on is where the ghost would fire again.
	s.tick(time.Now().Add(2 * time.Minute))

	if sub.calls != 1 {
		t.Errorf("fired %d times, want 1; the deleted schedule kept firing", sub.calls)
	}
	if _, err := store.Get(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() = %v, want ErrNotFound; the deleted schedule came back", err)
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]*Schedule{}, list, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("store is not empty after the delete (-want +got):\n%s", diff)
	}
}

// TestSchedulerConcurrentEditSurvivesFire covers the two quieter versions of the same write. A
// disable that re-enables itself and an edit that rolls back are both changes an operator made and
// watched take effect, only for the next fire to undo them.
//
// Each case also asserts the fire happened, so none of them can pass by the scheduler simply not
// firing at all.
func TestSchedulerConcurrentEditSurvivesFire(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Edit  func(ctx context.Context, store Store) error
		Check func(t *testing.T, got *Schedule)
	}{
		{
			Name: "a disable made during the fire holds",
			Edit: func(ctx context.Context, store Store) error {
				sc, err := store.Get(ctx, "s1")
				if err != nil {
					return err
				}
				sc.Enabled = false
				return store.Update(ctx, sc)
			},
			Check: func(t *testing.T, got *Schedule) {
				t.Helper()
				if got.Enabled {
					t.Error("the schedule is enabled again after an operator disabled it mid-fire")
				}
			},
		},
		{
			Name: "an edit made during the fire holds",
			Edit: func(ctx context.Context, store Store) error {
				sc, err := store.Get(ctx, "s1")
				if err != nil {
					return err
				}
				sc.Playbook = "edited.yml"
				return store.Update(ctx, sc)
			},
			Check: func(t *testing.T, got *Schedule) {
				t.Helper()
				if got.Playbook != "edited.yml" {
					t.Errorf("Playbook = %q, want edited.yml; the fire reverted the edit", got.Playbook)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			seedDue(t, store)

			sub := &editingSubmitter{runID: "run_x", during: func() {
				if err := test.Edit(ctx, store); err != nil {
					t.Errorf("edit during fire error = %v", err)
				}
			}}
			NewScheduler(store, sub, nil).tick(time.Now())

			if sub.calls != 1 {
				t.Fatalf("fired %d times, want 1; a case that never fires proves nothing", sub.calls)
			}
			got, err := store.Get(ctx, "s1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			test.Check(t, got)
			// The fire still has to be recorded, or the narrow write went too narrow.
			if got.LastRunID != "run_x" || got.LastRunAt == nil {
				t.Errorf("the fire was not recorded: last_run_id=%q last_run_at=%v",
					got.LastRunID, got.LastRunAt)
			}
		})
	}
}
