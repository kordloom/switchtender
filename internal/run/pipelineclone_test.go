package run

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestClonePipelineStepsSharesNothing changes every field of a cloned step, its dependency lists
// included, and checks the original is untouched.
func TestClonePipelineStepsSharesNothing(t *testing.T) {
	t.Parallel()
	original := []PipelineStep{{Name: "deploy", Tool: ToolBash, Command: "deploy",
		DependsOn: []string{"build"}, IfDenied: []string{"gate"}}}
	want := []PipelineStep{{Name: "deploy", Tool: ToolBash, Command: "deploy",
		DependsOn: []string{"build"}, IfDenied: []string{"gate"}}}
	clone := ClonePipelineSteps(original)
	clone[0].Name, clone[0].Command = "changed", "changed"
	clone[0].DependsOn[0], clone[0].IfDenied[0] = "changed", "changed"
	if diff := cmp.Diff(want, original); diff != "" {
		t.Errorf("original changed through the clone (-want +got):\n%s", diff)
	}
	if ClonePipelineSteps(nil) != nil {
		t.Error("ClonePipelineSteps(nil) is not nil")
	}
}
