package schedule

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// fullSchedule returns a schedule with every field set, for copy and encoding checks.
func fullSchedule(t *testing.T) *Schedule {
	t.Helper()
	next := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC)
	last := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	return &Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", Timezone: "America/Chicago",
		Playbook: "site.yml", Inventory: "prod", Shards: 4,
		Steps: []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "make"},
			{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"build"}, Retries: 2},
		},
		TemplateID: "tpl_1", OrgID: "org_1", Enabled: true,
		CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		NextRunAt: &next, CreatedBy: "operator", LastRunAt: &last, LastRunID: "run_1",
	}
}

// TestCloneIsIndependentOfTheOriginal pins that a stored schedule cannot be changed through a copy of
// it.
//
// Every read from the store hands back a clone, so a caller that edits what it was given must not be
// editing the row. A shared time pointer is the easy version of that mistake and would let a handler
// move a schedule's next fire time without saving anything.
func TestCloneIsIndependentOfTheOriginal(t *testing.T) {
	t.Parallel()
	original := fullSchedule(t)
	clone := original.Clone()

	if diff := cmp.Diff(original, clone, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("Clone() mismatch (-want +got):\n%s", diff)
	}
	if clone.NextRunAt == original.NextRunAt {
		t.Error("Clone() shares the next-run pointer, so moving it on a copy moves the stored one")
	}
	if clone.LastRunAt == original.LastRunAt {
		t.Error("Clone() shares the last-run pointer")
	}

	// Editing every scalar and the step slice on the copy leaves the original alone.
	moved := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clone.Name = "edited"
	clone.Cron = "* * * * *"
	clone.Enabled = false
	*clone.NextRunAt = moved
	*clone.LastRunAt = moved
	clone.Steps[0].Name = "edited"
	clone.Steps = append(clone.Steps, run.PipelineStep{Name: "extra"})

	if diff := cmp.Diff(fullSchedule(t), original, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("editing the clone changed the original (-want +got):\n%s", diff)
	}
}

// TestCloneHandlesTheEmptyShapes pins the copy at its boundaries, since a nil receiver and an absent
// slice are both ordinary states for a schedule that has never fired.
func TestCloneHandlesTheEmptyShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Schedule is the value to copy, nil for the nil case.
		Schedule *Schedule
	}{
		{Schedule: nil},                                    // Test 0: A nil schedule copies to nil, not a panic.
		{Schedule: &Schedule{}},                            // Test 1: The zero value.
		{Schedule: &Schedule{ID: "sch_1"}},                 // Test 2: Nothing but an id.
		{Schedule: &Schedule{Steps: nil}},                  // Test 3: No steps at all.
		{Schedule: &Schedule{Steps: []run.PipelineStep{}}}, // Test 4: An empty step slice.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := test.Schedule.Clone()
			if test.Schedule == nil {
				if got != nil {
					t.Errorf("Clone() of nil = %v, want nil", got)
				}
				return
			}
			if diff := cmp.Diff(test.Schedule, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Clone() mismatch (-want +got):\n%s", diff)
			}
			if got == test.Schedule {
				t.Error("Clone() returned the same pointer")
			}
		})
	}
}

// TestScheduleJSONRoundTrip pins that a schedule survives the encoding it is stored and served in.
//
// A field lost in transit is a schedule that runs something other than what it displays. The
// timezone and the owning organization are the two worst to lose: without the first a nightly job
// moves by hours, and without the second every run it fires becomes ownerless, which under strict
// grants means invisible to the tenant that scheduled it.
func TestScheduleJSONRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Schedule is the value to encode and decode.
		Schedule *Schedule
	}{
		{Schedule: fullSchedule(t)}, // Test 0: Every field set.
		{Schedule: &Schedule{ID: "sch_2", Cron: "@daily", Playbook: "p.yml"}}, // Test 1: The minimum.
		{Schedule: &Schedule{ID: "sch_3", Cron: "0 2 * * *", Steps: []run.PipelineStep{
			{Name: "a", Playbook: "a.yml"}, {Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}},
		}}}, // Test 2: A pipeline, whose graph is carried in the steps.
		{Schedule: &Schedule{ID: "sch_4", Name: "日本語 schedule", Cron: "0 2 * * *",
			Playbook: strings.Repeat("deep/", 200) + "site.yml"}}, // Test 3: Long and non-ASCII text.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(test.Schedule)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			var got Schedule
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if diff := cmp.Diff(test.Schedule, &got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNewIDIsUniqueAndPrefixed pins the identifier a schedule is addressed by.
//
// Two schedules sharing an id would have one overwrite the other on save, which for automation means
// a job silently replaced by another, and the prefix is what makes an id legible in a log line or a
// support conversation.
func TestNewIDIsUniqueAndPrefixed(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 500)
	for range 500 {
		id := NewID()
		if !strings.HasPrefix(id, "sch_") {
			t.Fatalf("NewID() = %q, want a sch_ prefix", id)
		}
		if len(id) <= len("sch_") {
			t.Fatalf("NewID() = %q, want random characters after the prefix", id)
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q, so one schedule would overwrite another", id)
		}
		seen[id] = true
	}
}

// TestMemStoreCopiesOnTheWayInAndOut pins that the in-memory store holds no pointer a caller can
// still reach, which is what makes it safe to hand the same schedule to two goroutines.
func TestMemStoreCopiesOnTheWayInAndOut(t *testing.T) {
	t.Parallel()
	store := NewMemStore()
	ctx := t.Context()
	sc := fullSchedule(t)
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// Mutating the schedule that was saved must not change the stored row.
	sc.Name = "edited after save"
	sc.Steps[0].Name = "edited after save"
	*sc.NextRunAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	got, err := store.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "nightly" || got.Steps[0].Name != "build" {
		t.Errorf("the store kept the caller's pointers: %+v", got)
	}
	if want := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC); !got.NextRunAt.Equal(want) {
		t.Errorf("NextRunAt = %v, want %v: the store shared the time pointer", got.NextRunAt, want)
	}
	// And the same on the way out.
	got.Name = "edited after get"
	again, err := store.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Name != "nightly" {
		t.Error("mutating the schedule from Get changed stored state")
	}
	// A listed schedule is a copy too, since the list is what the tick loop iterates.
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	list[0].Cron = "* * * * *"
	final, err := store.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if final.Cron != "0 2 * * *" {
		t.Errorf("Cron = %q, want the stored cadence: the list handed out the stored value", final.Cron)
	}
}
