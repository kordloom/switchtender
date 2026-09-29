package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

const (
	// escapeRepoPlaybook is the ordinary playbook the fixture repository commits.
	escapeRepoPlaybook = "site.yml"
	// escapeRepoInventory is the ordinary inventory the fixture repository commits.
	escapeRepoInventory = "hosts.ini"
	// escapeRepoSecret is the checkout-relative path that reaches, through a committed symlink, a
	// real file in a sibling directory the repository has no business reading.
	escapeRepoSecret = "esc/secret.txt"
	// escapeRepoRooted is the checkout-relative path that reaches a real file through a committed
	// symlink to the filesystem root.
	escapeRepoRooted = "slash/etc/hosts"
	// escapeRepoClimb is how many dot dot segments a committed escaping symlink climbs. It is more
	// than any checkout is deep, and dot dot at the filesystem root is the filesystem root, so one
	// committed link lands in the same place from the canonical checkout and from the isolated
	// per-run copy, whose depths below the cache differ.
	escapeRepoClimb = 24
	// escapeRepoTraversal climbs out of any checkout depth and lands on a real file. The dot dot
	// segments collapse against the filesystem root, so the result is "/etc/hosts" wherever the
	// isolated checkout happens to sit.
	escapeRepoTraversal = "../../../../../../../../etc/hosts"
	// escapeRepoAbsolute is an absolute path naming a real file.
	escapeRepoAbsolute = "/etc/hosts"
)

// newEscapeRepo builds a git repository committing an ordinary playbook and inventory alongside two
// symlinks that point out of the checkout: "esc" to a sibling directory holding a real file, and
// "slash" to the filesystem root. It returns the repository path.
//
// The links are committed rather than written into a checkout because the guards run on an isolated
// per-run copy. copyTree preserves symlinks, so a committed link survives both the clone and the
// copy and arrives exactly as a hostile repository would leave it.
//
// Both targets are spelled relatively, which is the only spelling that escapes a real sync. go-git
// checks a worktree out through a filesystem scoped to that worktree, so a committed symlink with
// an absolute target is written with the worktree root joined onto the front of it and lands back
// inside the checkout. A relative target is written verbatim, so that is the form that reaches a
// real file and the form the guard has to stop.
//
// Both targets are also files that exist. WithinRepo deliberately lets a path that resolves to
// nothing through on its lexical result, so a link to an absent file would be accepted with the
// guard deleted and the row would assert nothing.
func newEscapeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("stolen"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", secret, err)
	}

	dir := t.TempDir()
	for name, body := range map[string]string{
		escapeRepoPlaybook:  "---\n- hosts: all\n  tasks: []\n",
		escapeRepoInventory: "[web]\nweb01\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	sep := string(filepath.Separator)
	climb := strings.Repeat(".."+sep, escapeRepoClimb)
	for name, target := range map[string]string{
		"esc":   climb + strings.TrimPrefix(outside, sep),
		"slash": strings.TrimSuffix(climb, sep),
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	gitIn(t, dir, "init", "-b", "main")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "test")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "first")
	return dir
}

// gitIn runs a git command in dir and fails the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// escapedPaths returns the paths that reach outside cache, following symlinks the way a tool
// opening the file would. It is the set that must always be empty, because everything a run is
// handed has to live inside the project cache.
//
// A path that resolves to nothing is judged lexically, since no file can be opened through it. Both
// spellings of the cache count as inside it, because a resolved path and a lexical one disagree
// wherever the temp directory itself sits under a symlink, as it does on macOS.
func escapedPaths(cache string, paths ...string) []string {
	roots := []string{filepath.Clean(cache)}
	if resolved, err := filepath.EvalSymlinks(cache); err == nil {
		roots = append(roots, resolved)
	}
	var out []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		resolved := filepath.Clean(path)
		if r, err := filepath.EvalSymlinks(path); err == nil {
			resolved = r
		}
		if !underAny(resolved, roots) {
			out = append(out, path)
		}
	}
	return out
}

// underAny reports whether path is one of the roots or sits inside one.
func underAny(path string, roots []string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// liveCheckouts counts the checkouts still on disk under cache, one per copy of the fixture
// playbook. The canonical per-project checkout is one of them, so a run whose isolated copy has
// been cleaned up leaves exactly one behind.
func liveCheckouts(t *testing.T, cache string) int {
	t.Helper()
	found := 0
	err := filepath.WalkDir(cache, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // A directory removed under the walk is not a failure.
		}
		if !d.IsDir() && d.Name() == escapeRepoPlaybook {
			found++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s) error = %v", cache, err)
	}
	return found
}

// mustNotPanic calls f and reports a failure naming what panicked.
func mustNotPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", what, r)
		}
	}()
	f()
}

// TestResolveProjectRefusesPathsThatEscapeTheCheckout drives resolveProject over a real repository
// whose committed content reaches out of the checkout, and pins that no path the runner receives
// resolves outside the project cache.
//
// project.WithinRepo had a direct unit test and nothing exercised either of its two call sites
// here, so replacing both with a plain filepath.Join left the whole repository suite green. The
// consequence is not a wrong error message. spec.Playbook goes to ansible-playbook, which reads it,
// and spec.Inventory goes to ansible-inventory, which EXECUTES an inventory that is an executable
// file. A repository that commits a symlink climbing out of the checkout and names a path under it
// therefore chooses what the control node reads and, through the inventory, what it runs, as the
// executor.
func TestResolveProjectRefusesPathsThatEscapeTheCheckout(t *testing.T) {
	t.Parallel()
	repo := newEscapeRepo(t)

	tests := []struct {
		// Name describes the row.
		Name string
		// Playbook is the run's checkout-relative playbook path.
		Playbook string
		// Inventory is the run's checkout-relative inventory path, empty for none.
		Inventory string
		// Escapes is the path expected to be refused, empty when the row is allowed. Neither spec
		// path may end up naming it.
		Escapes string
		// Want is the error resolveProject must return.
		Want error
	}{{ // Test 0: A committed symlink to a sibling directory holding a real file.
		Name:     "playbook through a committed symlink to a sibling directory",
		Playbook: escapeRepoSecret, Escapes: escapeRepoSecret, Want: project.ErrEscapesRepo,
	}, { // Test 1: A committed symlink to the filesystem root.
		Name:     "playbook through a committed symlink to the filesystem root",
		Playbook: escapeRepoRooted, Escapes: "etc/hosts", Want: project.ErrEscapesRepo,
	}, { // Test 2: Dot dot segments that climb out of the checkout onto a real file.
		Name:     "playbook that traverses out of the checkout",
		Playbook: escapeRepoTraversal, Escapes: "etc/hosts", Want: project.ErrEscapesRepo,
	}, { // Test 3: An absolute path naming a real file.
		Name:     "playbook given as an absolute path",
		Playbook: escapeRepoAbsolute, Escapes: "etc/hosts", Want: project.ErrEscapesRepo,
	}, { // Test 4: The inventory is the path ansible-inventory would execute.
		Name:      "inventory through a committed symlink to a sibling directory",
		Playbook:  escapeRepoPlaybook,
		Inventory: escapeRepoSecret, Escapes: escapeRepoSecret, Want: project.ErrEscapesRepo,
	}, { // Test 5: The inventory reaching the filesystem root.
		Name:      "inventory through a committed symlink to the filesystem root",
		Playbook:  escapeRepoPlaybook,
		Inventory: escapeRepoRooted, Escapes: "etc/hosts", Want: project.ErrEscapesRepo,
	}, { // Test 6: The inventory traversing out of the checkout.
		Name:      "inventory that traverses out of the checkout",
		Playbook:  escapeRepoPlaybook,
		Inventory: escapeRepoTraversal, Escapes: "etc/hosts", Want: project.ErrEscapesRepo,
	}, { // Test 7: The inventory given as an absolute path.
		Name:      "inventory given as an absolute path",
		Playbook:  escapeRepoPlaybook,
		Inventory: escapeRepoAbsolute, Escapes: "etc/hosts", Want: project.ErrEscapesRepo,
	}, { // Test 8: Ordinary committed paths resolve into the checkout.
		Name:     "ordinary paths inside the checkout",
		Playbook: escapeRepoPlaybook, Inventory: escapeRepoInventory,
	}, { // Test 9: A path the sync never wrote passes through on the lexical result.
		Name:     "a playbook the sync never wrote",
		Playbook: "plays/never-written.yml",
	}, { // Test 10: The same for an inventory the sync never wrote.
		Name:     "an inventory the sync never wrote",
		Playbook: escapeRepoPlaybook, Inventory: "inventories/never-written.ini",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d: %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			cache := t.TempDir()
			syncer, err := project.NewSyncer(cache)
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			projects := project.NewMemStore()
			p := &project.Project{ID: "proj_esc", Name: "infra", RepoURL: repo, Branch: "main"}
			if err := projects.Save(context.Background(), p); err != nil {
				t.Fatalf("Save(project) error = %v", err)
			}
			d := New(run.NewMemStore(), roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					t.Error("the runner executed; resolveProject only rewrites the spec")
					return roundhouse.Result{}, nil
				}), zap.NewNop(), WithProjects(projects, syncer))
			defer d.Close()

			r := &run.Run{
				ID: "run_1", ProjectID: p.ID, Playbook: test.Playbook, Inventory: test.Inventory,
			}
			var spec roundhouse.Spec
			cleanup, err := d.resolveProject(r, &spec)

			// The doc comment promises a cleanup that is always safe to call, error paths included.
			// A caller that defers it on every return would panic exactly when a run was refused.
			if cleanup == nil {
				t.Fatalf("resolveProject() returned a nil cleanup alongside error %v, but its "+
					"contract says the cleanup is always safe to call", err)
			}

			// Nothing on the spec may resolve outside the project cache, whichever way the resolve
			// went. This is the assertion the returned error does not make on its own: once a path
			// reaches the spec, the reading and the executing have already been decided.
			escaped := escapedPaths(cache, spec.Playbook, spec.Inventory, spec.Dir)
			if diff := cmp.Diff([]string(nil), escaped, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the spec handed to the runner reaches outside the project cache %s, so "+
					"the repository chose what the control node reads (-want +got):\n%s", cache, diff)
			}
			if test.Escapes != "" {
				for name, got := range map[string]string{
					"Playbook": spec.Playbook, "Inventory": spec.Inventory,
				} {
					if got != "" && strings.HasSuffix(filepath.ToSlash(got), test.Escapes) {
						t.Errorf("spec.%s = %q, which still names the refused path %q",
							name, got, test.Escapes)
					}
				}
				// The refusal happens before the checkout is recorded, so a caller cannot fall back
				// to running in the directory anyway.
				if spec.Dir != "" {
					t.Errorf("spec.Dir = %q on a refused resolve, want empty", spec.Dir)
				}
			}

			// Every row reaches the sync, so an isolated copy is on disk next to the canonical
			// checkout and the cleanup has to remove it even when the resolve was refused.
			if got := liveCheckouts(t, cache); got != 2 {
				t.Errorf("checkouts under the cache before cleanup = %d, want 2: the canonical "+
					"checkout plus the isolated per-run copy", got)
			}
			mustNotPanic(t, "cleanup", cleanup)
			mustNotPanic(t, "a second cleanup", cleanup)
			if got := liveCheckouts(t, cache); got != 1 {
				t.Errorf("checkouts under the cache after cleanup = %d, want 1: the isolated copy "+
					"outlived the run", got)
			}

			if !errors.Is(err, test.Want) {
				t.Fatalf("resolveProject() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}

			// An allowed path is rewritten into the isolated per-run checkout, not the canonical
			// per-project one, so a later sync of the same project cannot change the files under a
			// run that is already reading them.
			if spec.Dir == "" {
				t.Fatal("spec.Dir was not set, so the run would execute outside the checkout")
			}
			if canonical := filepath.Join(cache, p.ID); spec.Dir == canonical {
				t.Errorf("spec.Dir = %q, the shared canonical checkout, not an isolated per-run copy",
					spec.Dir)
			}
			if want := filepath.Join(spec.Dir, test.Playbook); spec.Playbook != want {
				t.Errorf("spec.Playbook = %q, want %q", spec.Playbook, want)
			}
			wantInventory := ""
			if test.Inventory != "" {
				wantInventory = filepath.Join(spec.Dir, test.Inventory)
			}
			if spec.Inventory != wantInventory {
				t.Errorf("spec.Inventory = %q, want %q", spec.Inventory, wantInventory)
			}
			if len(r.CommitSHA) != 40 {
				t.Errorf("r.CommitSHA = %q, want the commit the run executes", r.CommitSHA)
			}
		})
	}
}

// TestHeldGitRunIsPinnedToTheCommitTheApproverSaw pins the gap between approval and execution.
//
// Approval binds the spec digest, but a git-backed spec names a branch, and a branch is a moving
// pointer: between the approver's yes and the worker's claim, HEAD can advance, so the release
// executed code nobody judged. The plan gate pinned its proposals; blanket policy holds did not. A
// held git run now records the commit that was current when it was held, the same field execution
// already refuses to move past.
func TestHeldGitRunIsPinnedToTheCommitTheApproverSaw(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newEscapeRepo(t)
	cache := t.TempDir()
	syncer, err := project.NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	p := &project.Project{ID: "proj_pin", Name: "infra", RepoURL: repo, Branch: "main"}
	if err := projects.Save(ctx, p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	d := New(run.NewMemStore(), roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), zap.NewNop(), WithProjects(projects, syncer), WithNoJanitor())
	defer d.Close()

	held, err := d.Submit(ctx, escapeRepoPlaybook, "",
		run.WithProject("proj_pin"), run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit(held) error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want pending_approval", held.Status)
	}
	if held.PinnedCommit == "" {
		t.Fatal("a held git run carries no pinned commit: approval would release whatever the " +
			"branch holds at execution time, which is the code nobody judged")
	}
	if held.Warning != "" {
		t.Errorf("a successful pin left a warning: %q", held.Warning)
	}

	// The pin travels to everything derived from the run.
	child := &run.Run{ID: "run_child"}
	inheritExecution(child, held)
	if child.PinnedCommit != held.PinnedCommit {
		t.Errorf("child pin = %q, want the parent's %q: a shard of a pinned run must execute the "+
			"judged commit", child.PinnedCommit, held.PinnedCommit)
	}

	// A run that is not held syncs and executes normally, unpinned.
	free, err := d.Submit(ctx, escapeRepoPlaybook, "", run.WithProject("proj_pin"))
	if err != nil {
		t.Fatalf("Submit(free) error = %v", err)
	}
	if free.PinnedCommit != "" {
		t.Errorf("an unheld run was pinned to %q; ordinary runs float on the branch by design",
			free.PinnedCommit)
	}
}
