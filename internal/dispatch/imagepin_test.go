package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/imageref"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// pinDigest is the digest the fake registry resolves every tag to.
var pinDigest = "sha256:" + strings.Repeat("9", 64)

// fakeImageResolver answers as a registry would, or fails as an unreachable one does.
type fakeImageResolver struct {
	// mu guards asked.
	mu sync.Mutex
	// Down makes every resolution fail.
	Down bool
	// Hang makes every resolution wait, the way a registry that never answers does, until the
	// lookup's context ends.
	Hang bool
	// asked holds each reference resolved.
	asked []string
}

// Digest returns pinDigest for any tag, fails when the registry is down, and waits for ctx to end
// when the registry hangs.
func (f *fakeImageResolver) Digest(ctx context.Context, ref string, _ imageref.Credentials) (string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, ref)
	f.mu.Unlock()
	if f.Hang {
		<-ctx.Done()
		return "", fmt.Errorf("%w: %w", imageref.ErrResolve, ctx.Err())
	}
	if f.Down {
		return "", fmt.Errorf("%w: the registry did not answer", imageref.ErrResolve)
	}
	return pinDigest, nil
}

// imageRunner records the image each execution was handed and reports pulled as the digest it ran.
type imageRunner struct {
	// mu guards images.
	mu sync.Mutex
	// images holds the image each execution was handed.
	images []string
	// pulled is the digest each execution reports.
	pulled string
}

// Run records the image and succeeds.
func (r *imageRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result,
	error) {
	r.mu.Lock()
	r.images = append(r.images, spec.Image)
	r.mu.Unlock()
	return roundhouse.Result{ExitCode: 0, ImageDigest: r.pulled}, nil
}

// seen returns the images executions were handed.
func (r *imageRunner) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.images...)
}

// TestSubmitPinsTheImageInForce covers which image a run is pinned to when it is submitted, and how:
// the run's own, then its project's, then the server default, each by the digest its tag resolves
// to when the registry answers, and by the tag alone when it does not.
func TestSubmitPinsTheImageInForce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// RunImage is the image the run or its template names.
		RunImage string
		// ProjectImage is the image its project names.
		ProjectImage string
		// DefaultImage is the server default.
		DefaultImage string
		// Down makes the registry unreachable.
		Down bool
		// Tool is the run's tool, ansible when empty.
		Tool string
		// WantImage is the image pinned onto the run.
		WantImage string
	}{{ // Test 0: The run's own image wins and is pinned by digest.
		Name: "run image", RunImage: "reg.example.com/runner:2", ProjectImage: "reg.example.com/proj:1",
		DefaultImage: "reg.example.com/default:1", WantImage: "reg.example.com/runner:2@" + pinDigest,
	}, { // Test 1: With none of its own, the project's image is pinned.
		Name: "project image", ProjectImage: "reg.example.com/proj:1",
		DefaultImage: "reg.example.com/default:1", WantImage: "reg.example.com/proj:1@" + pinDigest,
	}, { // Test 2: With neither, the server default is pinned.
		Name: "server default", DefaultImage: "reg.example.com/default:1",
		WantImage: "reg.example.com/default:1@" + pinDigest,
	}, { // Test 3: A registry that does not answer leaves the run bound to the tag.
		Name: "registry down", RunImage: "reg.example.com/runner:2", Down: true,
		WantImage: "reg.example.com/runner:2",
	}, { // Test 4: A run with no image anywhere runs on the host.
		Name: "no image", WantImage: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newPinFixture(t)
			if err := f.D.projects.Save(ctx, &project.Project{ID: "proj_pin", Name: "infra",
				RepoURL: f.Repo, Branch: "main", Image: test.ProjectImage}); err != nil {
				t.Fatalf("Save(project) error = %v", err)
			}
			resolver := &fakeImageResolver{Down: test.Down}
			d := New(run.NewMemStore(), okRunner(), nil, WithProjects(f.D.projects, f.D.syncer),
				WithDefaultImage(test.DefaultImage), WithImageResolver(resolver), WithNoJanitor(),
				WithClaimGate(func() error { return errNoClaim }))
			t.Cleanup(d.Close)
			opts := []run.SubmitOption{run.WithProject("proj_pin"), run.WithTool(run.ToolAnsible)}
			if test.RunImage != "" {
				opts = append(opts, run.WithImage(test.RunImage, ""))
			}
			r, err := d.Submit(ctx, escapeRepoPlaybook, "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if r.Image != test.WantImage {
				t.Errorf("pinned image = %q, want %q", r.Image, test.WantImage)
			}
		})
	}
}

// TestAnApprovedRunExecutesInTheImageItWasApprovedWith follows a held run whose project names its
// image. The image is pinned when the run is held, so the project's image changing afterward, which
// is an ordinary edit and goes through no approval, does not move the approved run into the new one.
func TestAnApprovedRunExecutesInTheImageItWasApprovedWith(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newPinFixture(t)
	if err := f.D.projects.Save(ctx, &project.Project{ID: "proj_pin", Name: "infra",
		RepoURL: f.Repo, Branch: "main", Image: "reg.example.com/trusted:1"}); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	store := run.NewMemStore()
	runner := &imageRunner{pulled: pinDigest}
	d := New(store, runner, nil, WithProjects(f.D.projects, f.D.syncer), WithAudits(audit.NewMemStore()),
		WithImageResolver(&fakeImageResolver{}), WithNoJanitor())
	t.Cleanup(d.Close)
	held, err := d.Submit(ctx, escapeRepoPlaybook, "", run.WithProject("proj_pin"),
		run.WithTool(run.ToolAnsible), run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if err := f.D.projects.Save(ctx, &project.Project{ID: "proj_pin", Name: "infra",
		RepoURL: f.Repo, Branch: "main", Image: "reg.example.com/attacker:1"}); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	if _, err := d.Approve(ctx, held.ID, decider("approver-1", "session")); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	final := waitTerminal(t, store, held.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", final.Status, final.Error)
	}
	want := []string{"reg.example.com/trusted:1@" + pinDigest}
	if got := runner.seen(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("the approved run executed in %q, want %q", got, want)
	}
	if final.ImageDigest != pinDigest {
		t.Errorf("recorded pulled digest = %q, want %q", final.ImageDigest, pinDigest)
	}
}

// TestAnApprovedRunWhoseImageChangedIsRefused covers the two ways an approved run's image can differ
// from the one it was approved with: the stored image rewritten under the approval, which the spec
// binding refuses, and an executor that would run it in its own default image when it was approved
// to run with none, which the image check refuses. Neither starts a tool.
func TestAnApprovedRunWhoseImageChangedIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Image is the image the run is held with.
		Image string
		// Rewrite is the image written under the approval, empty to leave it.
		Rewrite string
		// ExecutorDefault is the default image of the executor that claims it.
		ExecutorDefault string
		// WantError is what the refusal must say.
		WantError string
	}{{ // Test 0: The image rewritten after approval.
		Name: "rewritten", Image: "reg.example.com/trusted:1@" + pinDigest,
		Rewrite: "reg.example.com/attacker:1", WantError: "spec changed after it was approved",
	}, { // Test 1: Approved to run with no image, claimed by an executor with a default image.
		Name: "executor default", ExecutorDefault: "reg.example.com/worker-default:1",
		WantError: "approved to run in no image",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &imageRunner{}
			approver := New(store, runner, nil, WithAudits(audit.NewMemStore()), WithNoJanitor(),
				WithClaimGate(func() error { return errNoClaim }))
			t.Cleanup(approver.Close)
			opts := []run.SubmitOption{run.WithTool(run.ToolBash), run.WithCommand("true"),
				run.WithRequireApproval(true)}
			if test.Image != "" {
				opts = append(opts, run.WithImage(test.Image, ""))
			}
			held, err := approver.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if _, err := approver.Approve(ctx, held.ID, decider("approver-1", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			if test.Rewrite != "" {
				stored, err := store.Get(ctx, held.ID)
				if err != nil {
					t.Fatalf("Get() error = %v", err)
				}
				stored.Image = test.Rewrite
				if err := store.Save(ctx, stored); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			executor := New(store, runner, nil, WithDefaultImage(test.ExecutorDefault), WithNoJanitor())
			t.Cleanup(executor.Close)
			final := waitTerminal(t, store, held.ID)
			if final.Status != run.StatusFailed || !strings.Contains(final.Error, test.WantError) {
				t.Errorf("run ended %q (%q), want failed saying %q", final.Status, final.Error,
					test.WantError)
			}
			if got := runner.seen(); len(got) != 0 {
				t.Errorf("a tool ran in %q although the image changed after approval", got)
			}
		})
	}
}

// TestAnUnapprovedRunMayTakeTheExecutorsDefaultImage pins the one image an executor may still
// supply: its own default, for a run that pinned none when it was submitted and was never approved.
// That is the executor's environment, and the outcome records the image and the digest it pulled.
func TestAnUnapprovedRunMayTakeTheExecutorsDefaultImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &imageRunner{pulled: pinDigest}
	submitter := New(store, runner, nil, WithNoJanitor(), WithClaimGate(func() error { return errNoClaim }))
	t.Cleanup(submitter.Close)
	r, err := submitter.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("true"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	executor := New(store, runner, nil, WithDefaultImage("reg.example.com/worker-default:1"),
		WithNoJanitor())
	t.Cleanup(executor.Close)
	final := waitTerminal(t, store, r.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", final.Status, final.Error)
	}
	if final.Image != "reg.example.com/worker-default:1" || final.ImageDigest != pinDigest {
		t.Errorf("recorded image %q digest %q, want the executor default and its pulled digest",
			final.Image, final.ImageDigest)
	}
}

// TestASubmissionStopsWhenItsRequestEndsDuringTheImageLookup pins the image lookup to the request
// that submits the run. A registry that never answers holds the lookup only until that request
// ends, and the submission then stops with the request's error rather than storing a run nobody is
// waiting for.
func TestASubmissionStopsWhenItsRequestEndsDuringTheImageLookup(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithImageResolver(&fakeImageResolver{Hang: true}),
		WithDefaultImage("reg.example.com/runner:2"), WithNoJanitor(),
		WithClaimGate(func() error { return errNoClaim }))
	t.Cleanup(d.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("true"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Submit() error = %v, want the request's deadline", err)
	}
	runs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("a submission whose request ended stored %d runs, want none", len(runs))
	}
}
