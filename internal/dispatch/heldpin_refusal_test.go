package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAHeldRunWhoseCommitCannotBePinnedIsNotSaved holds a run over a project whose repository can
// no longer be fetched, through each submit path that holds one. The submission fails with the pin
// error and stores nothing: no run, no split parent or shard, and no pipeline, so nothing waits in
// front of an approver whose yes would release whatever the branch holds at execution time.
func TestAHeldRunWhoseCommitCannotBePinnedIsNotSaved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which submit path holds the run.
		Name string
		// Submit holds a run over proj_pin through one path.
		Submit func(ctx context.Context, d *Dispatcher) (*run.Run, error)
		// Want is the error the submission must fail with.
		Want error
	}{{ // Test 0: A single run.
		Name: "a single run",
		Submit: func(ctx context.Context, d *Dispatcher) (*run.Run, error) {
			return d.Submit(ctx, escapeRepoPlaybook, "", run.WithProject("proj_pin"),
				run.WithRequireApproval(true))
		},
		Want: ErrCommitUnpinned,
	}, { // Test 1: A split across two hosts, whose parent is the held run.
		Name: "a split",
		Submit: func(ctx context.Context, d *Dispatcher) (*run.Run, error) {
			return d.SubmitSplit(ctx, escapeRepoPlaybook, escapeRepoInventory, 2,
				run.WithProject("proj_pin"), run.WithRequireApproval(true))
		},
		Want: ErrCommitUnpinned,
	}, { // Test 2: A pipeline, whose parent is the held run.
		Name: "a pipeline",
		Submit: func(ctx context.Context, d *Dispatcher) (*run.Run, error) {
			return d.SubmitPipeline(ctx, "release", "",
				[]run.PipelineStep{{Name: "deploy", Playbook: escapeRepoPlaybook}},
				run.WithProject("proj_pin"), run.WithRequireApproval(true))
		},
		Want: ErrCommitUnpinned,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newPinFixture(t)
			// The repository stops being one, so the sync at hold time fails.
			if err := os.RemoveAll(filepath.Join(f.Repo, ".git")); err != nil {
				t.Fatalf("RemoveAll() error = %v", err)
			}

			got, err := test.Submit(ctx, f.D)

			if !errors.Is(err, test.Want) {
				t.Fatalf("submit error = %v, want %v", err, test.Want)
			}
			if !strings.Contains(err.Error(), "try again") {
				t.Errorf("error = %q, want it to tell the submitter to try again", err)
			}
			if got != nil {
				t.Errorf("submit returned run %s with status %q, want none", got.ID, got.Status)
			}
			stored, err := f.Store.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(stored) != 0 {
				t.Errorf("the store holds %d runs, want 0: the first is %s, %q with pin %q and "+
					"warning %q", len(stored), stored[0].ID, stored[0].Status,
					stored[0].PinnedCommit, stored[0].Warning)
			}
			if n := f.Runner.executions.Load(); n != 0 {
				t.Errorf("the runner ran %d times, want 0", n)
			}
		})
	}
}
