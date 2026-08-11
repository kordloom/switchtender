package schedule

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// fakeSubmitter records how it was fired for scheduler tests.
type fakeSubmitter struct {
	// runID is returned as the created run id.
	runID string
	// calls counts total submissions.
	calls int
	// kind records which submit path was taken most recently.
	kind string
}

// Submit records a single submission.
func (f *fakeSubmitter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run, error) {
	f.calls++
	f.kind = "single"
	return &run.Run{ID: f.runID}, nil
}

// SubmitSplit records a split submission.
func (f *fakeSubmitter) SubmitSplit(context.Context, string, string, int, ...run.SubmitOption) (*run.Run, error) {
	f.calls++
	f.kind = "split"
	return &run.Run{ID: f.runID}, nil
}

// SubmitPipeline records a pipeline submission.
func (f *fakeSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep, ...run.SubmitOption) (*run.Run, error) {
	f.calls++
	f.kind = "pipeline"
	return &run.Run{ID: f.runID}, nil
}

func TestSchedulerTickFires(t *testing.T) {
	t.Parallel()
	store := NewMemStore()
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	if err := store.Save(ctx, &Schedule{
		ID: "s1", Cron: "* * * * *", Playbook: "p.yml", Enabled: true,
		CreatedAt: time.Now(), NextRunAt: &past,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &fakeSubmitter{runID: "run_x"}

	NewScheduler(store, sub, nil).tick(time.Now())

	if sub.calls != 1 || sub.kind != "single" {
		t.Errorf("fired calls=%d kind=%q, want 1 single", sub.calls, sub.kind)
	}
	got, err := store.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastRunID != "run_x" {
		t.Errorf("LastRunID = %q, want run_x", got.LastRunID)
	}
	if got.LastRunAt == nil {
		t.Error("LastRunAt not set")
	}
	if got.NextRunAt == nil || !got.NextRunAt.After(time.Now()) {
		t.Error("NextRunAt not advanced to the future")
	}
}

func TestSchedulerTickSkips(t *testing.T) {
	t.Parallel()
	store := NewMemStore()
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Minute)
	_ = store.Save(ctx, &Schedule{ID: "future", Cron: "* * * * *", Playbook: "p", Enabled: true, NextRunAt: &future})
	_ = store.Save(ctx, &Schedule{ID: "disabled", Cron: "* * * * *", Playbook: "p", Enabled: false, NextRunAt: &past})
	sub := &fakeSubmitter{runID: "r"}

	NewScheduler(store, sub, nil).tick(time.Now())

	if sub.calls != 0 {
		t.Errorf("fired %d, want 0 since one is in the future and one is disabled", sub.calls)
	}
}

func TestSchedulerFireRouting(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-time.Minute)
	tests := []struct {
		Sc       *Schedule
		WantKind string
	}{
		{ // Test 0: Steps route to a pipeline.
			Sc: &Schedule{ID: "p", Cron: "* * * * *", Enabled: true, NextRunAt: &past,
				Steps: []run.PipelineStep{{Name: "a", Playbook: "a.yml"}}}, WantKind: "pipeline",
		},
		{ // Test 1: Shards route to a split.
			Sc: &Schedule{ID: "s", Cron: "* * * * *", Enabled: true, NextRunAt: &past,
				Playbook: "p.yml", Shards: 3}, WantKind: "split",
		},
		{ // Test 2: A plain playbook routes to a single run.
			Sc: &Schedule{ID: "o", Cron: "* * * * *", Enabled: true, NextRunAt: &past,
				Playbook: "p.yml"}, WantKind: "single",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := NewMemStore()
			if err := store.Save(context.Background(), test.Sc); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &fakeSubmitter{runID: "r"}
			NewScheduler(store, sub, nil).tick(time.Now())
			if sub.kind != test.WantKind {
				t.Errorf("kind = %q, want %q", sub.kind, test.WantKind)
			}
		})
	}
}

func TestSchedulerFiresTemplate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "preset", Playbook: "preset.yml", Inventory: "inv.ini",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save template error = %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if err := store.Save(ctx, &Schedule{
		ID: "sch_t", Cron: "@every 1h", TemplateID: "tpl_1", Enabled: true,
		CreatedAt: time.Now(), NextRunAt: &past,
	}); err != nil {
		t.Fatalf("Save schedule error = %v", err)
	}

	sub := &fakeSubmitter{runID: "run_t"}
	s := NewScheduler(store, sub, zap.NewNop(), WithTemplates(templates))
	s.tick(time.Now())

	if sub.calls != 1 || sub.kind != "single" {
		t.Fatalf("calls = %d kind = %q, want one single submit", sub.calls, sub.kind)
	}
	got, err := store.Get(ctx, "sch_t")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastRunID != "run_t" {
		t.Errorf("LastRunID = %q, want run_t", got.LastRunID)
	}
}

// TestSchedulerFiresWorkflowTemplate proves a schedule targeting a saved workflow template fires its
// graph as a pipeline, not as an empty single run.
func TestSchedulerFiresWorkflowTemplate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_wf", Name: "nightly-ship", Inventory: "prod",
		Steps: []run.PipelineStep{
			{Name: "build", Tool: "bash", Command: "make"},
			{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"build"}},
		},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save template error = %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if err := store.Save(ctx, &Schedule{
		ID: "sch_wf", Cron: "@every 1h", TemplateID: "tpl_wf", Enabled: true,
		CreatedAt: time.Now(), NextRunAt: &past,
	}); err != nil {
		t.Fatalf("Save schedule error = %v", err)
	}

	sub := &fakeSubmitter{runID: "run_wf"}
	s := NewScheduler(store, sub, zap.NewNop(), WithTemplates(templates))
	s.tick(time.Now())

	if sub.calls != 1 || sub.kind != "pipeline" {
		t.Fatalf("calls = %d kind = %q, want one pipeline submit; a scheduled workflow template "+
			"did not fire its graph", sub.calls, sub.kind)
	}
}
