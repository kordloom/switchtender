package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// pinFixture is a dispatcher over a one-project git repository, which every held-pin test needs.
type pinFixture struct {
	// D is the dispatcher, with the project store and syncer wired in.
	D *Dispatcher
	// Store is the run store behind D.
	Store run.Store
	// Runner counts executions, so a test can prove a refused step ran nothing.
	Runner *countingRunnerLister
	// Repo is the repository path, for advancing the branch.
	Repo string
	// Head is the commit the branch held when the fixture was built.
	Head string
}

// newPinFixture builds a git repository, a project named proj_pin over its main branch, and a
// dispatcher that can sync it and split across two hosts.
func newPinFixture(t *testing.T) pinFixture {
	t.Helper()
	repo := newEscapeRepo(t)
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	p := &project.Project{ID: "proj_pin", Name: "infra", RepoURL: repo, Branch: "main"}
	if err := projects.Save(context.Background(), p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01", "web02"}}
	d := New(store, runner, zap.NewNop(), WithProjects(projects, syncer), WithNoJanitor())
	t.Cleanup(d.Close)
	return pinFixture{D: d, Store: store, Runner: runner, Repo: repo, Head: gitHead(t, repo)}
}

// gitHead returns the commit the repository's HEAD names.
func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestEveryHeldSubmissionPinsItsCommit pins the approval-time commit on every path a git-backed run
// is held through. A held run records the commit that was current when it was held, so an approval
// releases the code the approver read and not whatever the branch holds later. The single-run,
// split, and retry paths pinned it. The pipeline path did not, so a held pipeline's steps inherited
// an empty pin, and approving it ran whatever had been pushed to the branch in the meantime.
func TestEveryHeldSubmissionPinsItsCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which submit path holds the run.
		Name string
		// Submit holds a run over proj_pin through one path.
		Submit func(ctx context.Context, d *Dispatcher) (*run.Run, error)
	}{{ // Test 0: A single run.
		Name: "a single run",
		Submit: func(ctx context.Context, d *Dispatcher) (*run.Run, error) {
			return d.Submit(ctx, escapeRepoPlaybook, "", run.WithProject("proj_pin"),
				run.WithRequireApproval(true))
		},
	}, { // Test 1: A split across two hosts.
		Name: "a split",
		Submit: func(ctx context.Context, d *Dispatcher) (*run.Run, error) {
			return d.SubmitSplit(ctx, escapeRepoPlaybook, escapeRepoInventory, 2,
				run.WithProject("proj_pin"), run.WithRequireApproval(true))
		},
	}, { // Test 2: A pipeline, the path that never pinned.
		Name: "a pipeline",
		Submit: func(ctx context.Context, d *Dispatcher) (*run.Run, error) {
			return d.SubmitPipeline(ctx, "release", "",
				[]run.PipelineStep{{Name: "deploy", Playbook: escapeRepoPlaybook}},
				run.WithProject("proj_pin"), run.WithRequireApproval(true))
		},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newPinFixture(t)

			held, err := test.Submit(ctx, f.D)
			if err != nil {
				t.Fatalf("submit error = %v", err)
			}
			if held.Status != run.StatusPendingApproval {
				t.Fatalf("status = %q, want pending_approval", held.Status)
			}
			if held.PinnedCommit != f.Head {
				t.Errorf("PinnedCommit = %q, want the commit the branch held at the hold, %q: "+
					"approval would release whatever the branch holds when it runs",
					held.PinnedCommit, f.Head)
			}
			if held.Warning != "" {
				t.Errorf("a successful pin left a warning: %q", held.Warning)
			}
			stored, err := f.Store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if stored.PinnedCommit != f.Head {
				t.Errorf("stored PinnedCommit = %q, want %q: the record an approval releases is "+
					"the stored one, so a pin that never reached it binds nothing",
					stored.PinnedCommit, f.Head)
			}
		})
	}
}

// TestAnApprovedPipelineRunsOnlyTheCommitItWasHeldAt follows a held pipeline through its approval.
// If nothing was pushed in between, the step runs the commit the approver saw. If the branch moved,
// the step refuses and nothing executes, which is the same answer a single held run gives.
func TestAnApprovedPipelineRunsOnlyTheCommitItWasHeldAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Moved reports whether a commit lands on the branch between the hold and the approval.
		Moved bool
		// WantStatus is how the approved pipeline must finish.
		WantStatus run.Status
		// WantExecutions is how many times the runner may have run.
		WantExecutions int64
	}{{ // Test 0: Nothing moved, so the step runs the commit the approver saw.
		Moved: false, WantStatus: run.StatusSucceeded, WantExecutions: 1,
	}, { // Test 1: The branch moved, so the step refuses rather than run code nobody judged.
		Moved: true, WantStatus: run.StatusFailed, WantExecutions: 0,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newPinFixture(t)

			held, err := f.D.SubmitPipeline(ctx, "release", "",
				[]run.PipelineStep{{Name: "deploy", Playbook: escapeRepoPlaybook}},
				run.WithProject("proj_pin"), run.WithRequireApproval(true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			if test.Moved {
				changed := "---\n- hosts: all\n  tasks:\n    - ansible.builtin.ping:\n"
				path := filepath.Join(f.Repo, escapeRepoPlaybook)
				if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
				gitIn(t, f.Repo, "commit", "-am", "second")
			}
			if _, err := f.D.Approve(ctx, held.ID, "reviewer", "session"); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			waitForStatus(t, f.Store, held.ID, test.WantStatus)

			steps, err := f.Store.Steps(ctx, held.ID)
			if err != nil {
				t.Fatalf("Steps() error = %v", err)
			}
			if len(steps) != 1 {
				t.Fatalf("the pipeline has %d step runs, want 1", len(steps))
			}
			if steps[0].PinnedCommit != f.Head {
				t.Errorf("step PinnedCommit = %q, want the pipeline's %q", steps[0].PinnedCommit,
					f.Head)
			}
			if test.Moved && !strings.Contains(steps[0].Error, "approved against commit") {
				t.Errorf("step error = %q, want it to name the commit it was approved against",
					steps[0].Error)
			}
			if got := f.Runner.executions.Load(); got != test.WantExecutions {
				t.Errorf("the runner ran %d times, want %d", got, test.WantExecutions)
			}
		})
	}
}
