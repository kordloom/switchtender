package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// Project trees the regrade tests push.
var (
	// forcedRole rewrites the web role to restart the service for real even under check mode.
	forcedRole = map[string]string{
		"roles/web/tasks/main.yml": "- name: Restart web\n  ansible.builtin.service: name=nginx " +
			"state=restarted\n  check_mode: false\n",
	}
	// restartRole puts the web role back to the restart restartProject ships.
	restartRole = map[string]string{
		"roles/web/tasks/main.yml": restartProject["roles/web/tasks/main.yml"],
	}
	// readmeOnly adds a file no playbook reads.
	readmeOnly = map[string]string{"README.md": "infra\n"}
)

// pushingProjects is a project store that lands a push on the project's repository the first time
// the project is read once its checkout exists. On a submission that is the read just ahead of a
// held run's pin, after the gate's own fetch has brought the checkout in, so the gate and the pin
// fetch two different commits.
type pushingProjects struct {
	// Store is the store the projects are read from.
	project.Store
	// Syncer holds the checkout whose existence arms the push.
	Syncer *project.Syncer
	// Push lands the push.
	Push func()
	// mu guards pushed.
	mu sync.Mutex
	// pushed reports whether Push ran.
	pushed bool
}

// Get lands the push when the checkout of id exists and it has not landed yet, then reads the
// project.
func (s *pushingProjects) Get(ctx context.Context, id string) (*project.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pushed && s.Syncer.ReadCommit(id, "", func(fs.FS, string) error { return nil }) == nil {
		s.pushed = true
		s.Push()
	}
	return s.Store.Get(ctx, id)
}

// regradeFixture is a dispatcher over one git project for the regrade tests.
type regradeFixture struct {
	// D is the dispatcher.
	D *Dispatcher
	// Syncer keeps D's project checkouts.
	Syncer *project.Syncer
	// Project is the one project, proj_regrade.
	Project *project.Project
	// Repo is the project's repository.
	Repo string
}

// newRegradeFixture builds a project over a repository holding files and a dispatcher deciding by
// rules, with no policy store when rules is nil. When push is not nil, it is committed to the
// repository between the gate's fetch and the pin's.
func newRegradeFixture(t *testing.T, files map[string]string, rules policy.Store,
	push map[string]string) regradeFixture {
	t.Helper()
	ctx := context.Background()
	repo := newTreeRepo(t, files)
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	p := &project.Project{ID: "proj_regrade", Name: "infra", RepoURL: repo, Branch: "main"}
	if err := projects.Save(ctx, p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	if push != nil {
		projects = &pushingProjects{Store: projects, Syncer: syncer,
			Push: func() { commitTree(t, repo, push) }}
	}
	opts := []Option{WithProjects(projects, syncer), WithNoJanitor()}
	if rules != nil {
		opts = append(opts, WithPolicies(rules))
	}
	d := New(run.NewMemStore(), &countingRunnerLister{hosts: []string{"web01", "web02"}},
		zap.NewNop(), opts...)
	t.Cleanup(d.Close)
	return regradeFixture{D: d, Syncer: syncer, Project: p, Repo: repo}
}

// ruleStore returns a policy store holding rules, saved in order.
func ruleStore(t *testing.T, rules ...*policy.Policy) policy.Store {
	t.Helper()
	store := policy.NewMemStore()
	for _, p := range rules {
		p.MaxDestroy = policy.DisabledMaxDestroy
		if err := store.Save(context.Background(), p); err != nil {
			t.Fatalf("Save(%s) error = %v", p.ID, err)
		}
	}
	return store
}

// distinctRules holds everything, and asks for a second person only for a dry run that does real
// work, which is decided by what the commit's playbook forces under check mode. The blanket rule is
// listed first, so it is the one a hold names whatever the commit holds.
func distinctRules(t *testing.T) policy.Store {
	t.Helper()
	return ruleStore(t,
		&policy.Policy{ID: "pol_a", Name: "hold everything", Effect: policy.EffectRequireApproval},
		&policy.Policy{ID: "pol_b", Name: "two people for real work",
			Effect: policy.EffectRequireApproval, ExcludeDryRun: true, RequireDistinctApprover: true})
}

// submitter submits one kind of run against the regrade fixture's project.
type submitter func(ctx context.Context, d *Dispatcher, projectID string,
	opts ...run.SubmitOption) (*run.Run, error)

// submitSingle submits a single run of site.yml.
func submitSingle(ctx context.Context, d *Dispatcher, projectID string,
	opts ...run.SubmitOption) (*run.Run, error) {
	return d.Submit(ctx, "site.yml", "hosts.ini", append(opts, run.WithProject(projectID))...)
}

// submitWorkflow submits a workflow whose one step runs site.yml.
func submitWorkflow(ctx context.Context, d *Dispatcher, projectID string,
	opts ...run.SubmitOption) (*run.Run, error) {
	return d.SubmitPipeline(ctx, "release", "hosts.ini",
		[]run.PipelineStep{{Name: "deploy", Tool: run.ToolAnsible, Playbook: "site.yml"}},
		append(opts, run.WithProject(projectID))...)
}

// agentCheck is the option set of an agent's check-mode run.
func agentCheck() []run.SubmitOption {
	return []run.SubmitOption{run.WithActor("ops-agent"), run.WithActorType(policy.ActorKindAgent),
		run.WithDryRun(true)}
}

// TestAHeldRunsScanReadsThePinnedCommit covers the scan an approver reads. The gate reads the
// project's checkout as its last sync left it, and the pin fetches the branch again, so a push the
// checkout has not seen, or a project never synced at all, left the approver reading a scan of
// another commit, or of nothing, beside an approval that binds the pinned one.
func TestAHeldRunsScanReadsThePinnedCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files    map[string]string
		Push     map[string]string
		PreSync  bool
		WantFind string
	}{{ // Test 0: Synced before a push that forces a role, then launched with no policy store.
		Files: restartProject, Push: forcedRole, PreSync: true, WantFind: "check_mode to false",
	}, { // Test 1: A project never synced before its first run.
		Files: merged(restartProject, forcedRole), PreSync: false, WantFind: "check_mode to false",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newRegradeFixture(t, test.Files, nil, nil)
			if test.PreSync {
				wt, err := f.Syncer.Sync(f.Project, "")
				if err != nil {
					t.Fatalf("Sync() error = %v", err)
				}
				wt.Cleanup()
			}
			if test.Push != nil {
				commitTree(t, f.Repo, test.Push)
			}
			r, err := submitSingle(ctx, f.D, f.Project.ID, agentCheck()...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if r.Status != run.StatusPendingApproval {
				t.Fatalf("status = %q, want the agent's run held", r.Status)
			}
			head := gitHead(t, f.Repo)
			if r.PinnedCommit != head {
				t.Fatalf("PinnedCommit = %q, want the branch head %q", r.PinnedCommit, head)
			}
			if len(r.DryRunScans) == 0 {
				t.Fatalf("the held run records no scan")
			}
			scan := r.DryRunScans[0]
			if !strings.Contains(scan.Source, r.PinnedCommit[:12]) ||
				!strings.Contains(scan.Source, "pinned to") {
				t.Errorf("scan source = %q, want the commit the run is pinned to, %s", scan.Source,
					r.PinnedCommit[:12])
			}
			if len(scan.Unread) != 0 {
				t.Errorf("scan unread = %q, want the pinned commit read", scan.Unread)
			}
			if got := strings.Join(scan.Findings, "\n"); !strings.Contains(got, test.WantFind) {
				t.Errorf("scan findings = %q, want one containing %q", got, test.WantFind)
			}
		})
	}
}

// TestAHeldRunIsRefusedWhenThePinMovesTheDecision covers a push that lands between the gate's
// fetch and the pin's. The gate decided on one commit and the approval would bind another, so when
// the rules decide differently at the pinned commit the run is refused rather than held on a
// decision about code it will not execute.
func TestAHeldRunIsRefusedWhenThePinMovesTheDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Rules        func(t *testing.T) policy.Store
		Submit       submitter
		Files        map[string]string
		Push         map[string]string
		DryRun       bool
		WantHeldBy   string
		WantDistinct bool
		Want         error
	}{{ // Test 0: Held at the gate's commit for forcing real work, which the pinned commit reverts.
		Rules: excludeDryRuns, Submit: submitSingle, Files: merged(restartProject, forcedRole),
		Push: restartRole, DryRun: true, Want: ErrCommitUnpinned,
	}, { // Test 1: The pinned commit forces real work under check mode, which needs a second person.
		Rules: distinctRules, Submit: submitSingle, Files: restartProject, Push: forcedRole,
		DryRun: true, Want: ErrCommitUnpinned,
	}, { // Test 2: The same, for a workflow whose step runs the playbook.
		Rules: distinctRules, Submit: submitWorkflow, Files: restartProject, Push: forcedRole,
		DryRun: true, Want: ErrCommitUnpinned,
	}, { // Test 3: Nothing moves, and the run is held.
		Rules: distinctRules, Submit: submitSingle, Files: restartProject, DryRun: true,
		WantHeldBy: "hold everything", Want: nil,
	}, { // Test 4: Nothing moves, and the workflow is held.
		Rules: distinctRules, Submit: submitWorkflow, Files: restartProject, DryRun: true,
		WantHeldBy: "hold everything", Want: nil,
	}, { // Test 5: A push no rule reads, and the run is held at the pushed commit.
		Rules: distinctRules, Submit: submitSingle, Files: restartProject, Push: readmeOnly,
		DryRun: true, WantHeldBy: "hold everything", Want: nil,
	}, { // Test 6: Nothing moves on forced work, and the second person is still asked for.
		Rules: distinctRules, Submit: submitWorkflow, Files: merged(restartProject, forcedRole),
		DryRun: true, WantHeldBy: "hold everything", WantDistinct: true, Want: nil,
	}, { // Test 7: A workflow held at the gate's commit for forced work the pinned commit reverts.
		Rules: excludeDryRuns, Submit: submitWorkflow, Files: merged(restartProject, forcedRole),
		Push: restartRole, DryRun: true, Want: ErrCommitUnpinned,
	}, { // Test 8: Nothing moves on forced work, and the run is held for it.
		Rules: excludeDryRuns, Submit: submitSingle, Files: merged(restartProject, forcedRole),
		DryRun: true, WantHeldBy: "prod ansible", Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newRegradeFixture(t, test.Files, test.Rules(t), test.Push)
			r, err := test.Submit(context.Background(), f.D, f.Project.ID, run.WithActor("alice"),
				run.WithDryRun(test.DryRun))
			if !errors.Is(err, test.Want) {
				t.Fatalf("submit error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if r.Status != run.StatusPendingApproval {
				t.Fatalf("status = %q, want held", r.Status)
			}
			head := gitHead(t, f.Repo)
			if r.PinnedCommit != head {
				t.Errorf("PinnedCommit = %q, want the branch head %q", r.PinnedCommit, head)
			}
			if r.RequireDistinctApprover != test.WantDistinct {
				t.Errorf("RequireDistinctApprover = %v, want %v", r.RequireDistinctApprover,
					test.WantDistinct)
			}
			if r.HeldByPolicy != test.WantHeldBy {
				t.Errorf("HeldByPolicy = %q, want %q", r.HeldByPolicy, test.WantHeldBy)
			}
		})
	}
}

// TestAFailedGateFetchRefusesTheRun covers a gate whose fetch fails under a rule that reads what a
// run executes. Graded on the checkout's last sync, a run nobody held was claimed later by a sync
// that could succeed and execute a commit no rule had read. A run under no such rule never fetches
// at the gate, so a remote that cannot be reached changes nothing for it.
func TestAFailedGateFetchRefusesTheRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Rules  func(t *testing.T) policy.Store
		Submit submitter
		Want   error
	}{{ // Test 0: A change-free dry run nobody holds, under a rule that excludes dry runs.
		Rules: excludeDryRuns, Submit: submitSingle, Want: ErrCommitUnpinned,
	}, { // Test 1: The same, for a workflow.
		Rules: excludeDryRuns, Submit: submitWorkflow, Want: ErrCommitUnpinned,
	}, { // Test 2: A run under no rule that reads what it executes.
		Rules:  func(t *testing.T) policy.Store { t.Helper(); return ruleStore(t) },
		Submit: submitSingle, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newRegradeFixture(t, restartProject, test.Rules(t), nil)
			wt, err := f.Syncer.Sync(f.Project, "")
			if err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			wt.Cleanup()
			if err := os.RemoveAll(f.Repo); err != nil {
				t.Fatalf("RemoveAll(repo) error = %v", err)
			}
			r, err := test.Submit(context.Background(), f.D, f.Project.ID, run.WithActor("alice"),
				run.WithDryRun(true))
			if !errors.Is(err, test.Want) {
				t.Fatalf("submit error = %v, want %v", err, test.Want)
			}
			if test.Want == nil && r.Status != run.StatusPending {
				t.Errorf("status = %q, want the run saved to run", r.Status)
			}
		})
	}
}
