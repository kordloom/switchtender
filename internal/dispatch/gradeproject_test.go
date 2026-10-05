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

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// Project trees the gate tests assemble repositories from.
var (
	// restartProject is a project whose playbook runs one role, which restarts a service.
	restartProject = map[string]string{
		"site.yml":                 "- hosts: all\n  roles:\n    - web\n",
		"hosts.ini":                "[web]\nweb01\nweb02\n",
		"roles/web/tasks/main.yml": "- ansible.builtin.service:\n    name: nginx\n    state: restarted\n",
	}
	// teardownRole rewrites that role to remove an archive for good, one hop from the playbook.
	teardownRole = map[string]string{
		"roles/web/tasks/main.yml": "- ansible.builtin.file:\n    path: /srv/archive\n    state: absent\n",
	}
)

// writeTree writes files under dir, creating directories as needed.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", rel, err)
		}
	}
}

// newTreeRepo builds a git repository on main holding files and returns its path.
func newTreeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	writeTree(t, dir, files)
	gitIn(t, dir, "init", "-b", "main")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "test")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "first")
	return dir
}

// commitTree writes files into repo and commits them, the way a push lands on the branch.
func commitTree(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	writeTree(t, repo, files)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-m", "next")
}

// merged returns the union of trees, later ones winning.
func merged(trees ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, tree := range trees {
		for k, v := range tree {
			out[k] = v
		}
	}
	return out
}

// gateFixture is a dispatcher over one git project, with a rule holding anything that cannot be
// undone.
type gateFixture struct {
	// D is the dispatcher.
	D *Dispatcher
	// Store is the run store behind D.
	Store run.Store
	// Syncer keeps D's project checkouts.
	Syncer *project.Syncer
	// Project is the one project, proj_gate.
	Project *project.Project
	// Repo is the project's repository, for pushing to.
	Repo string
}

// newGateFixture builds a project over a repository holding files, and a dispatcher whose one rule
// holds irreversible runs.
func newGateFixture(t *testing.T, files map[string]string) gateFixture {
	t.Helper()
	ctx := context.Background()
	repo := newTreeRepo(t, files)
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	p := &project.Project{ID: "proj_gate", Name: "infra", RepoURL: repo, Branch: "main"}
	if err := projects.Save(ctx, p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	rules := policy.NewMemStore()
	if err := rules.Save(ctx, &policy.Policy{ID: "pol_perm", Name: "hold the permanent",
		Effect: policy.EffectRequireApproval, Reversibility: run.Irreversible,
		MaxDestroy: policy.DisabledMaxDestroy}); err != nil {
		t.Fatalf("Save(policy) error = %v", err)
	}
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, zap.NewNop(),
		WithProjects(projects, syncer), WithPolicies(rules), WithNoJanitor())
	t.Cleanup(d.Close)
	return gateFixture{D: d, Store: store, Syncer: syncer, Project: p, Repo: repo}
}

// TestTheGateReadsWhatAProjectRunPullsIn is the case the whole path exists for. A project's
// playbook names a role and the role holds the work, so the gate has to read the role, in the
// project, to hold a teardown and let a restart through.
func TestTheGateReadsWhatAProjectRunPullsIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files    map[string]string
		WantHeld bool
	}{{ // Test 0: A role that tears down an archive is held.
		Files: merged(restartProject, teardownRole), WantHeld: true,
	}, { // Test 1: A role that restarts a service goes through.
		Files: restartProject, WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newGateFixture(t, test.Files)
			r, err := f.D.Submit(context.Background(), "site.yml", "hosts.ini",
				run.WithProject(f.Project.ID))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := r.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Errorf("held = %v, want %v: the gate graded %q without reading the role it runs",
					held, test.WantHeld, r.Playbook)
			}
		})
	}
}

// TestTheGateGradesTheCommitAboutToRun covers the push that lands just before a launch.
//
// The gate reads the project's checkout, and a checkout holds what its last sync fetched. A
// teardown pushed after an earlier run synced the project, and launched straight away, was graded
// on the commit before it and ran past the rule written to hold it. Every submission path has to
// fetch before it grades, so each one is driven here against a checkout left stale on purpose.
func TestTheGateGradesTheCommitAboutToRun(t *testing.T) {
	t.Parallel()
	origins := []struct {
		Name string
		Fire func(ctx context.Context, d *Dispatcher, projectID string) (*run.Run, error)
	}{{ // Test 0: A single run.
		Name: "a single run",
		Fire: func(ctx context.Context, d *Dispatcher, projectID string) (*run.Run, error) {
			return d.Submit(ctx, "site.yml", "hosts.ini", run.WithProject(projectID))
		},
	}, { // Test 1: A split across two hosts.
		Name: "a split",
		Fire: func(ctx context.Context, d *Dispatcher, projectID string) (*run.Run, error) {
			return d.SubmitSplit(ctx, "site.yml", "hosts.ini", 2, run.WithProject(projectID))
		},
	}, { // Test 2: A pipeline whose step runs the playbook.
		Name: "a pipeline",
		Fire: func(ctx context.Context, d *Dispatcher, projectID string) (*run.Run, error) {
			return d.SubmitPipeline(ctx, "release", "hosts.ini",
				[]run.PipelineStep{{Name: "deploy", Tool: run.ToolAnsible, Playbook: "site.yml"}},
				run.WithProject(projectID))
		},
	}}
	for testNum, o := range origins {
		t.Run(fmt.Sprintf("test %d %s", testNum, o.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newGateFixture(t, restartProject)
			// An earlier run synced the project while it only restarted a service.
			wt, err := f.Syncer.Sync(f.Project, "")
			if err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			wt.Cleanup()
			commitTree(t, f.Repo, teardownRole)

			r, err := o.Fire(ctx, f.D, f.Project.ID)
			if err != nil {
				t.Fatalf("submit error = %v", err)
			}
			if r.Status != run.StatusPendingApproval {
				t.Fatalf("%s of a freshly pushed teardown was not held: the gate graded the commit "+
					"before it", o.Name)
			}
			// And the approver is shown the grade of the commit the run is pinned to, which is the
			// one it will execute.
			stored, err := f.Store.Get(ctx, r.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			undo := run.AssessReversibilityFrom(stored, run.ReversibilityEvidence{
				Playbook: ScanRunPlaybook(firstPlaybookRun(t, stored), f.Syncer)})
			said := strings.Join(undo.Reasons, "\n")
			if undo.Class != run.Irreversible || !strings.Contains(said, "roles/web/tasks/main.yml") ||
				!strings.Contains(said, "the commit this run is pinned to") {
				t.Errorf("the held run reads as %q:\n%s", undo.Class, said)
			}
		})
	}
}

// firstPlaybookRun returns r, or for a pipeline its first step, which is where a pipeline keeps
// the playbook it runs: the parent's own playbook field holds the pipeline's name. The step is
// built the way the dispatcher builds it, so it carries the parent's project and pinned commit.
func firstPlaybookRun(t *testing.T, r *run.Run) *run.Run {
	t.Helper()
	if r.Kind != run.KindPipeline || len(r.Steps) == 0 {
		return r
	}
	return stepRun(r, r.Steps[0], 0, 0, nil)
}

// TestAProjectRunIsNeverGradedFromTheServersDirectory pins the stray read this path replaced. A
// project run names its playbook relative to the project, and it was opened relative to the
// server's working directory, so a file of the same name beside the server was graded in place of
// the project's own.
//
// It changes the process's working directory, so it cannot run in parallel with anything.
func TestAProjectRunIsNeverGradedFromTheServersDirectory(t *testing.T) {
	beside := t.TempDir()
	writeTree(t, beside, map[string]string{"site.yml": "- hosts: all\n  tasks:\n" +
		"    - ansible.builtin.shell: rm -rf /var/lib/data\n"})
	t.Chdir(beside)

	r := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml", ProjectID: "proj_gate"}
	if got := ScanRunPlaybook(r, nil); got != nil {
		t.Errorf("a project run with no checkout was graded from %s/site.yml: %v", beside, got.Permanent)
	}

	f := newGateFixture(t, restartProject)
	wt, err := f.Syncer.Sync(f.Project, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wt.Cleanup()
	got := ScanRunPlaybook(r, f.Syncer)
	if got == nil || len(got.Permanent) > 0 {
		t.Errorf("a project run was graded from the server's directory instead of its project: %+v", got)
	}
}

// TestOnlyARuleThatReadsThePlaybookFetches holds the cost of the gate's fetch to the installs that
// use it. A reversibility floor reads what any playbook does, and for a dry run a dry-run exclusion
// and a risk floor read it too, since the playbook decides whether the dry run forces real work.
// Nothing else does, so an install without such a rule must not pay a fetch on every submission.
func TestOnlyARuleThatReadsThePlaybookFetches(t *testing.T) {
	t.Parallel()
	apply, dry := &run.Run{Playbook: "site.yml"}, &run.Run{Playbook: "site.yml", DryRun: true}
	tests := []struct {
		Rules []*policy.Policy
		Run   *run.Run
		Want  bool
	}{{ // Test 0: No rules at all.
		Rules: nil, Run: apply, Want: false,
	}, { // Test 1: A risk floor, which reads no playbook for a real run.
		Rules: []*policy.Policy{{Name: "prod", MinRisk: "high"}}, Run: apply, Want: false,
	}, { // Test 2: A reversibility floor.
		Rules: []*policy.Policy{{Name: "perm", Reversibility: run.Irreversible}}, Run: apply, Want: true,
	}, { // Test 3: A reversibility floor beside a nil entry.
		Rules: []*policy.Policy{nil, {Name: "perm", Reversibility: run.ReversibleCostly}}, Run: apply,
		Want: true,
	}, { // Test 4: A dry-run exclusion reads the playbook of a dry run.
		Rules: []*policy.Policy{{Name: "prod", ExcludeDryRun: true}}, Run: dry, Want: true,
	}, { // Test 5: And not of a real run, which it never exempts.
		Rules: []*policy.Policy{{Name: "prod", ExcludeDryRun: true}}, Run: apply, Want: false,
	}, { // Test 6: A risk floor reads a dry run's playbook, since it grades low only if clean.
		Rules: []*policy.Policy{{Name: "prod", MinRisk: "medium"}}, Run: dry, Want: true,
	}, { // Test 7: A plain rule reads nothing, dry run or not.
		Rules: []*policy.Policy{{Name: "prod", Tool: "ansible"}}, Run: dry, Want: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := readsPlaybook(test.Rules, test.Run); got != test.Want {
				t.Errorf("readsPlaybook() = %v, want %v", got, test.Want)
			}
		})
	}
}

// TestAHeldRunKeepsTheGradeOfItsPinnedCommit covers the approver's view after the branch moves. A
// held run executes the commit it was pinned to or nothing, so its grade has to stay the grade of
// that commit when later pushes change the branch, while a run submitted now is graded on the new
// commit.
func TestAHeldRunKeepsTheGradeOfItsPinnedCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newGateFixture(t, merged(restartProject, teardownRole))
	held, err := f.D.Submit(ctx, "site.yml", "hosts.ini", run.WithProject(f.Project.ID))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want the teardown held", held.Status)
	}

	// The teardown is reverted on the branch, and a sync fetches the revert.
	role := "roles/web/tasks/main.yml"
	commitTree(t, f.Repo, map[string]string{role: restartProject[role]})
	if _, err := f.Syncer.Fetch(f.Project, ""); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	stored, err := f.Store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	pinned := run.AssessReversibilityFrom(stored,
		run.ReversibilityEvidence{Playbook: ScanRunPlaybook(stored, f.Syncer)})
	if pinned.Class != run.Irreversible {
		t.Errorf("the held run now grades %q: its approver would be shown the branch's revert, "+
			"not the teardown the run will execute", pinned.Class)
	}
	fresh := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml", ProjectID: f.Project.ID}
	now := run.AssessReversibilityFrom(fresh,
		run.ReversibilityEvidence{Playbook: ScanRunPlaybook(fresh, f.Syncer)})
	if now.Class != run.ReversibleCostly {
		t.Errorf("a run submitted now grades %q, want the reverted branch's %q", now.Class,
			run.ReversibleCostly)
	}
}
