package dispatch

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/run"
)

// TestSubmitPipelineLeavesTheCallersStepsAlone submits a step whose command holds a NUL byte, which
// saving replaces, and checks the caller's slice still holds what it passed. The parent used to keep
// the caller's slice itself, so the cleanup rewrote it, and two parallel submissions of one
// definition raced on it.
func TestSubmitPipelineLeavesTheCallersStepsAlone(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), dispatchtest.OKRunner(), zap.NewNop(), WithNoJanitor())
	t.Cleanup(d.Close)
	steps := []run.PipelineStep{{Name: "build", Tool: run.ToolBash, Command: "echo a\x00b"}}
	if _, err := d.SubmitPipeline(context.Background(), "release", "", steps); err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if got := steps[0].Command; got != "echo a\x00b" {
		t.Errorf("the caller's step command became %q, want it unchanged", got)
	}
}

// TestACleanedSubmissionStillVerifies pins that a keyed install seals a workflow and its step over
// the text the store will hold, not the text it was handed. Saving replaces a NUL byte, so a seal
// made over the bytes as submitted refused the workflow's own honest step as rewritten.
func TestACleanedSubmissionStillVerifies(t *testing.T) {
	t.Parallel()
	ci := newChildInstall(t)
	steps := []run.PipelineStep{{Name: "deploy", Tool: run.ToolBash, Command: "echo a\x00b"}}
	parent, err := ci.d.SubmitPipeline(context.Background(), "release", "", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got, step := ci.finished(t, parent.ID)
	if got.Status != run.StatusSucceeded || step == nil || step.Status != run.StatusSucceeded {
		t.Fatalf("workflow = %s (%s), want it and its step succeeded", got.Status, got.Error)
	}
	if n := ci.runner.count("echo a�b"); n != 1 {
		t.Errorf("the cleaned command ran %d times, want once", n)
	}
}
