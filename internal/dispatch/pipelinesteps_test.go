package dispatch

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestSubmitPipelineLeavesTheCallersStepsAlone submits a step whose command holds a NUL byte, which
// saving replaces, and checks the caller's slice still holds what it passed. The parent used to keep
// the caller's slice itself, so the cleanup rewrote it, and two parallel submissions of one
// definition raced on it.
func TestSubmitPipelineLeavesTheCallersStepsAlone(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithNoJanitor())
	t.Cleanup(d.Close)
	steps := []run.PipelineStep{{Name: "build", Tool: run.ToolBash, Command: "echo a\x00b"}}
	if _, err := d.SubmitPipeline(context.Background(), "release", "", steps); err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if got := steps[0].Command; got != "echo a\x00b" {
		t.Errorf("the caller's step command became %q, want it unchanged", got)
	}
}
