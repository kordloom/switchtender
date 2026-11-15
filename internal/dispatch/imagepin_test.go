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

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/imageref"
	"github.com/kordloom/switchtender/internal/policy"
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
			d := New(run.NewMemStore(), dispatchtest.OKRunner(), nil, WithProjects(f.D.projects, f.D.syncer),
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

// TestSubmitRefusesAnUnpinnedImage covers a dispatcher that refuses unpinned images: a tag the
// registry does not pin, or any tag when no resolver is set, refuses the submission, while a pinned
// image, a tag the registry pins, and a run on the host submit as before.
func TestSubmitRefusesAnUnpinnedImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// RunImage is the image the run names.
		RunImage string
		// Down makes the registry unreachable.
		Down bool
		// NoResolver leaves the dispatcher without a resolver, as --image-digest-lookup off does.
		NoResolver bool
		// WantImage is the image pinned onto the run when it submits.
		WantImage string
		// Want is the error the submission ends in.
		Want error
	}{{ // Test 0: A registry that does not answer refuses the submission.
		Name: "registry down", RunImage: "reg.example.com/runner:2", Down: true, Want: ErrImageUnpinned,
	}, { // Test 1: With no resolver, a tag cannot be pinned and is refused.
		Name: "no resolver", RunImage: "reg.example.com/runner:2", NoResolver: true,
		Want: ErrImageUnpinned,
	}, { // Test 2: A tag the registry pins submits by its digest.
		Name: "registry pins", RunImage: "reg.example.com/runner:2",
		WantImage: "reg.example.com/runner:2@" + pinDigest,
	}, { // Test 3: An image already pinned by digest needs no resolver.
		Name: "already pinned", RunImage: "reg.example.com/runner@" + pinDigest, NoResolver: true,
		WantImage: "reg.example.com/runner@" + pinDigest,
	}, { // Test 4: A run with no image runs on the host and is not refused.
		Name: "no image", NoResolver: true, WantImage: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newPinFixture(t)
			if err := f.D.projects.Save(ctx, &project.Project{ID: "proj_pin", Name: "infra",
				RepoURL: f.Repo, Branch: "main"}); err != nil {
				t.Fatalf("Save(project) error = %v", err)
			}
			opts := []Option{WithProjects(f.D.projects, f.D.syncer), WithRefuseUnpinnedImage(true),
				WithNoJanitor(), WithClaimGate(func() error { return errNoClaim })}
			if !test.NoResolver {
				opts = append(opts, WithImageResolver(&fakeImageResolver{Down: test.Down}))
			}
			store := run.NewMemStore()
			d := New(store, dispatchtest.OKRunner(), nil, opts...)
			t.Cleanup(d.Close)
			sopts := []run.SubmitOption{run.WithProject("proj_pin"), run.WithTool(run.ToolAnsible)}
			if test.RunImage != "" {
				sopts = append(sopts, run.WithImage(test.RunImage, ""))
			}
			r, err := d.Submit(ctx, escapeRepoPlaybook, "", sopts...)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Submit() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				if !strings.Contains(err.Error(), "--allow-unpinned-image") {
					t.Errorf("refusal %q does not name the flag that allows it", err)
				}
				if runs, lerr := store.List(ctx); lerr != nil || len(runs) != 0 {
					t.Errorf("stored runs = %d (%v), want none for a refused submission", len(runs), lerr)
				}
				return
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
	final := dispatchtest.WaitTerminal(t, store, held.ID)
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
			final := dispatchtest.WaitTerminal(t, store, held.ID)
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
	final := dispatchtest.WaitTerminal(t, store, r.ID)
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
	d := New(store, dispatchtest.OKRunner(), nil, WithImageResolver(&fakeImageResolver{Hang: true}),
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

// loginRecorder is a registry that records the login each digest lookup carried.
type loginRecorder struct {
	// mu guards asked.
	mu sync.Mutex
	// asked holds the login each lookup carried, in order.
	asked []imageref.Credentials
}

// Digest records the login and answers with pinDigest.
func (l *loginRecorder) Digest(_ context.Context, _ string, creds imageref.Credentials) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, creds)
	return pinDigest, nil
}

// logins returns the login each lookup carried.
func (l *loginRecorder) logins() []imageref.Credentials {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]imageref.Credentials(nil), l.asked...)
}

// TestAPullLoginLeavesOnlyForItsOwnRegistry covers what a submission hands the registry its image
// names. The digest lookup runs before any rule weighs the run, so it is the one place a secret
// could leave for a host the submitter chose on a run that is then held. A credential of any kind
// but registry is refused as a pull credential before the lookup, and a registry login goes only to
// the host its own settings bind it to.
func TestAPullLoginLeavesOnlyForItsOwnRegistry(t *testing.T) {
	t.Parallel()
	const (
		image  = "registry.other.example/x:1"
		secret = "PULL-SECRET-BODY"
	)
	tests := []struct {
		// Name labels the case.
		Name string
		// Kind is the kind of the credential the run names as its pull credential.
		Kind credential.Kind
		// Host is the registry host the credential's settings bind it to, none when empty.
		Host string
		// Deny adds a rule that covers every bash run.
		Deny bool
		// WantLogins is the login each registry lookup carried.
		WantLogins []imageref.Credentials
		// Want is the error the submission returns.
		Want error
	}{{ // Test 0: An agent's run naming an SSH key as its pull credential is refused, nothing sent.
		Name: "ssh key on an agent run", Kind: credential.KindSSHKey, Want: credential.ErrPullKind,
	}, { // Test 1: The same run under a rule that covers it is refused the same way.
		Name: "ssh key under a rule", Kind: credential.KindSSHKey, Deny: true,
		Want: credential.ErrPullKind,
	}, { // Test 2: An env credential is no pull credential either.
		Name: "env credential", Kind: credential.KindEnv, Want: credential.ErrPullKind,
	}, { // Test 3: A registry login bound to another host stays home and the lookup goes ahead.
		Name: "bound to another registry", Kind: credential.KindRegistry, Host: "ghcr.io",
		WantLogins: []imageref.Credentials{{}},
	}, { // Test 4: A registry login with no host bound is sent nowhere.
		Name: "bound to nothing", Kind: credential.KindRegistry,
		WantLogins: []imageref.Credentials{{}},
	}, { // Test 5: A registry login bound to the image's own registry is sent there, named for it.
		Name: "bound to this registry", Kind: credential.KindRegistry, Host: "registry.other.example",
		WantLogins: []imageref.Credentials{{
			Username: "puller", Password: secret, Host: "registry.other.example",
		}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			sealer := credential.NewSealer("pass", "salt")
			sealed, err := sealer.Seal("puller\n" + secret)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			named := &credential.Credential{ID: "cred_named", Name: "named", Kind: test.Kind,
				Secret: sealed}
			if test.Host != "" {
				named.Settings = map[string]string{credential.SettingRegistryHost: test.Host}
			}
			creds := credential.NewMemStore()
			if err := creds.Save(ctx, named); err != nil {
				t.Fatalf("Save(credential) error = %v", err)
			}
			rules := policy.NewMemStore()
			if test.Deny {
				if err := rules.Save(ctx, &policy.Policy{ID: "pol_deny", Name: "no bash",
					Effect: policy.EffectDeny, Tool: run.ToolBash,
					MaxDestroy: policy.DisabledMaxDestroy}); err != nil {
					t.Fatalf("Save(policy) error = %v", err)
				}
			}
			rec := &loginRecorder{}
			d := New(run.NewMemStore(), dispatchtest.OKRunner(), nil, WithCredentials(creds, sealer),
				WithPolicies(rules), WithImageResolver(rec), WithRefuseUnpinnedImage(true),
				WithNoJanitor(), WithClaimGate(func() error { return errNoClaim }))
			t.Cleanup(d.Close)
			r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("true"),
				run.WithActor("ci-agent"), run.WithActorType("agent"),
				run.WithImage(image, named.ID))
			if !errors.Is(err, test.Want) {
				t.Fatalf("Submit() error = %v, want %v", err, test.Want)
			}
			if test.Want == nil && r.Status != run.StatusPendingApproval {
				t.Errorf("status = %q, want the agent's run held", r.Status)
			}
			if diff := cmp.Diff(test.WantLogins, rec.logins()); diff != "" {
				t.Errorf("logins sent to the registry mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
