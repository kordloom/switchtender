package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// moduleTree writes a module tree the way a download leaves one: a manifest and a module's files.
func moduleTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "modules")
	writeFiles(t, dir, files)
	return dir
}

// pinnedTree is a downloaded tree the cases below vary.
func pinnedTree() map[string]string {
	return map[string]string{
		"modules.json": `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
			`{"Key":"net","Source":"registry.example.test/acme/net/null","Version":"1.0.0",` +
			`"Dir":".terraform/modules/net"}]}`,
		"net/main.tf": "output \"o\" {\n  value = \"one\"\n}\n",
	}
}

// TestAModuleTreeHasOneDigest covers what the digest the run is held to covers. Two downloads of
// the same modules digest the same, though a repository download leaves version control metadata
// that differs every time and the tool may order its manifest differently. A change to any file, to
// whether a file is executable, or to where a link points changes it. A link leaving the tree and
// anything that is not a file, a directory, or a link cannot be pinned at all.
//
//nolint:funlen // Test function.
func TestAModuleTreeHasOneDigest(t *testing.T) {
	t.Parallel()
	base, err := moduleTreeDigest(moduleTree(t, pinnedTree()))
	if err != nil || !strings.HasPrefix(base, "sha256:") {
		t.Fatalf("moduleTreeDigest() = %q, %v", base, err)
	}
	tests := []struct {
		// Change alters the tree after it is written.
		Change func(t *testing.T, dir string)
		// WantSame is whether the digest stays the base one.
		WantSame bool
		// Want is the error, nil for none.
		Want error
	}{{ // Test 0: Version control metadata from a repository download is left out.
		Change: func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"net/.git/index": "stat data", "net/.git/HEAD": "x"})
		},
		WantSame: true,
	}, { // Test 1: The manifest is read for what it records, not the order it was written in.
		Change: func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"modules.json": `{"Modules": [` +
				`{"Key":"net","Source":"registry.example.test/acme/net/null","Version":"1.0.0",` +
				`"Dir":".terraform/modules/net"}, {"Key":"","Source":"","Dir":"."}]}`})
		},
		WantSame: true,
	}, { // Test 2: A file's content.
		Change: func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"net/main.tf": "output \"o\" {\n  value = \"two\"\n}\n"})
		},
	}, { // Test 3: The version the manifest records.
		Change: func(t *testing.T, dir string) {
			writeFiles(t, dir, map[string]string{"modules.json": strings.Replace(
				pinnedTree()["modules.json"], "1.0.0", "1.1.0", 1)})
		},
	}, { // Test 4: Whether a file is executable.
		Change: func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "net", "main.tf"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}, { // Test 5: A link inside the tree is recorded by where it points.
		Change: func(t *testing.T, dir string) {
			if err := os.Symlink("main.tf", filepath.Join(dir, "net", "alias.tf")); err != nil {
				t.Fatal(err)
			}
		},
	}, { // Test 6: A link leaving the tree cannot be pinned.
		Change: func(t *testing.T, dir string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, "abs")); err != nil {
				t.Fatal(err)
			}
		},
		Want: errModulesUnpinned,
	}, { // Test 7: Nor can one climbing out by a relative path.
		Change: func(t *testing.T, dir string) {
			if err := os.Symlink("../../outside", filepath.Join(dir, "net", "up")); err != nil {
				t.Fatal(err)
			}
		},
		Want: errModulesUnpinned,
	}, { // Test 8: Nor anything that is not a file, a directory, or a link.
		Change: func(t *testing.T, dir string) {
			if err := makeFIFO(filepath.Join(dir, "net", "pipe")); err != nil {
				t.Skipf("no named pipes here: %v", err)
			}
		},
		Want: errModulesUnpinned,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := moduleTree(t, pinnedTree())
			test.Change(t, dir)
			got, err := moduleTreeDigest(dir)
			if !errors.Is(err, test.Want) {
				t.Fatalf("moduleTreeDigest() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if (got == base) != test.WantSame {
				t.Errorf("digest %q against base %q, want same = %v", got, base, test.WantSame)
			}
		})
	}
}

// TestTheGateKeepsWhatItRead covers the copy of the module tree the gate keeps for the run. It
// comes back whole, in place of whatever the working directory held, and only while it is current.
// A copy that no longer has the digest it is kept under is dropped rather than used, and a working
// directory whose .terraform links outside itself is never written through.
//
//nolint:funlen // Test function.
func TestTheGateKeepsWhatItRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Prepare sets up the store and the working directory after the tree is kept, and returns a
		// directory nothing may be written into, empty for none.
		Prepare func(t *testing.T, archive, workdir string) string
		// WantRestored is whether the tree comes back.
		WantRestored bool
		// WantError is whether restoring reports a problem.
		WantError bool
		// WantDropped is whether the kept copy is gone afterward.
		WantDropped bool
	}{{ // Test 0: The kept tree replaces whatever the working directory held.
		Prepare: func(t *testing.T, _, workdir string) string {
			writeFiles(t, workdir, map[string]string{".terraform/modules/stale/main.tf": "old"})
			return ""
		},
		WantRestored: true,
	}, { // Test 1: Past its time it is not offered.
		Prepare: func(t *testing.T, archive, _ string) string {
			old := time.Now().Add(-DefaultModuleKeepFor - time.Hour)
			if err := os.Chtimes(archive, old, old); err != nil {
				t.Fatal(err)
			}
			return ""
		},
	}, { // Test 2: A copy whose content changed is dropped, not used.
		Prepare: func(t *testing.T, archive, _ string) string {
			changed := pinnedTree()
			changed["net/main.tf"] = "output \"o\" {\n  value = \"two\"\n}\n"
			f, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			if err := archiveModuleTree(f, moduleTree(t, changed)); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			return ""
		},
		WantError: true, WantDropped: true,
	}, { // Test 3: A .terraform linking outside the working directory is not written through.
		Prepare: func(t *testing.T, _, workdir string) string {
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(workdir, ".terraform")); err != nil {
				t.Fatal(err)
			}
			return outside
		},
		WantError: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tree := moduleTree(t, pinnedTree())
			digest, err := moduleTreeDigest(tree)
			if err != nil {
				t.Fatal(err)
			}
			s := newModuleStore(t.TempDir(), DefaultModuleKeepFor, DefaultModuleKeepMaxBytes)
			if err := s.keep(digest, tree); err != nil {
				t.Fatalf("keep() error = %v", err)
			}
			archive, _ := s.archivePath(digest)
			workdir := t.TempDir()
			outside := test.Prepare(t, archive, workdir)
			restored, err := s.restore(digest, workdir)
			if restored != test.WantRestored || (err != nil) != test.WantError {
				t.Fatalf("restore() = %v, %v, want %v with error %v", restored, err,
					test.WantRestored, test.WantError)
			}
			if _, err := os.Stat(archive); errors.Is(err, os.ErrNotExist) != test.WantDropped {
				t.Errorf("kept copy present = %v, want dropped %v", err == nil, test.WantDropped)
			}
			if outside != "" {
				if left, _ := os.ReadDir(outside); len(left) != 0 {
					t.Errorf("restoring wrote through the link outside the working directory: %v", left)
				}
			}
			if !test.WantRestored {
				return
			}
			modules := filepath.Join(workdir, ".terraform", "modules")
			got, err := moduleTreeDigest(modules)
			if err != nil || got != digest {
				t.Errorf("restored digest = %q, %v, want %q", got, err, digest)
			}
			if _, err := os.Stat(filepath.Join(modules, "stale")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("what the working directory held before was left beside the kept tree: %v", err)
			}
		})
	}
	// Past the store's bound, the oldest copy goes first, and never the one just kept.
	s := newModuleStore(t.TempDir(), DefaultModuleKeepFor, DefaultModuleKeepMaxBytes)
	first := moduleTree(t, pinnedTree())
	d1, _ := moduleTreeDigest(first)
	if err := s.keep(d1, first); err != nil {
		t.Fatal(err)
	}
	firstPath, _ := s.archivePath(d1)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(firstPath, old, old); err != nil {
		t.Fatal(err)
	}
	changed := pinnedTree()
	changed["net/main.tf"] = "changed"
	second := moduleTree(t, changed)
	d2, _ := moduleTreeDigest(second)
	s.maxBytes = 1
	if err := s.keep(d2, second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the oldest copy outlived the store's bound: %v", err)
	}
	if secondPath, _ := s.archivePath(d2); !fileExists(secondPath) {
		t.Error("the copy just kept was dropped to make room")
	}
}

// fileExists reports whether a file is at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// seeingRunner stands in for the plan: it records the module output the working directory holds and
// whether init was told the modules are already in place.
type seeingRunner struct {
	// mu guards seen and installed.
	mu sync.Mutex
	// seen is the module's main.tf as the plan found it, empty when the plan never ran.
	seen string
	// installed is the spec's ModulesInstalled.
	installed bool
}

// Run records what the plan would have used.
func (s *seeingRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	body, _ := os.ReadFile(filepath.Join(spec.Dir, spec.Command, ".terraform", "modules", "net",
		"main.tf"))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen, s.installed = string(body), spec.ModulesInstalled
	return roundhouse.Result{ExitCode: 0}, nil
}

// TestAPlanRunsTheModulesTheGateRead proves a held plan executes the module code the gate read and
// an approver released, or nothing, when a newer version of a module appears while it waits. The
// gate downloads version 1.0.0 with real terraform and keeps it. 1.1.0 is then published, which the
// configuration's constraint allows, so a fresh init would install it. Approved, the plan runs the
// kept 1.0.0 with init told to install nothing. With the kept copy gone, as after it expires or on
// another executor, the run downloads again, finds 1.1.0, and is refused with why. With nothing
// published, the fresh download matches and the plan runs.
//
//nolint:funlen // Test function.
func TestAPlanRunsTheModulesTheGateRead(t *testing.T) {
	t.Parallel()
	one := "output \"o\" {\n  value = \"one\"\n}\n"
	tests := []struct {
		// Publish publishes 1.1.0 after the gate read 1.0.0.
		Publish bool
		// Forget drops the copy the gate kept.
		Forget bool
		// WantStatus is how the run ends.
		WantStatus run.Status
		// WantSeen is the module the plan used, empty when it never ran.
		WantSeen string
		// WantError is a fragment of why the run was refused.
		WantError string
	}{{ // Test 0: A newer release published while the plan waited is not what runs.
		Publish: true, WantStatus: run.StatusSucceeded, WantSeen: one,
	}, { // Test 1: With the kept copy gone, the newer release is found and the run refused.
		Publish: true, Forget: true, WantStatus: run.StatusFailed,
		WantError: "refused: the modules this run would use are not the ones the gate read",
	}, { // Test 2: With the kept copy gone and nothing newer, the same modules download and run.
		Forget: true, WantStatus: run.StatusSucceeded, WantSeen: one,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			reg := newTestRegistry(t, map[string]registryModule{
				"drifts": {Files: map[string]string{"main.tf": one}}})
			creds, sealer := cliCredential(t, reg.cliConfig(t))
			repo := newTreeRepo(t, map[string]string{"infra/main.tf": registryCall("drifts")})
			syncer, err := project.NewSyncer(t.TempDir())
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			projects := project.NewMemStore()
			p := &project.Project{ID: "proj_pin", Name: "infra", RepoURL: repo, Branch: "main"}
			if err := projects.Save(ctx, p); err != nil {
				t.Fatalf("Save(project) error = %v", err)
			}
			plan := &seeingRunner{}
			fetch := newFetchingRunner(t)
			fetch.Runner = plan
			store := run.NewMemStore()
			files := t.TempDir()
			d := New(store, fetch, zap.NewNop(), WithProjects(projects, syncer),
				WithPolicies(rulesHolding(t, &policy.Policy{ID: "pol_all", Name: "hold plans",
					Tool: run.ToolTerraform, Effect: policy.EffectRequireApproval,
					MaxDestroy: policy.DisabledMaxDestroy})),
				WithCredentials(creds, sealer), WithRunFilesRoot(files), WithNoJanitor(),
				WithClaimInterval(5*time.Millisecond))
			t.Cleanup(d.Close)
			wt, err := syncer.Sync(p, "")
			if err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			wt.Cleanup()

			held, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
				run.WithCommand("infra"), run.WithProject(p.ID), run.WithDryRun(true),
				run.WithCredentialIDs([]string{"cred_tf"}))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held.Status != run.StatusPendingApproval || !strings.HasPrefix(held.ModulesDigest(),
				"sha256:") {
				t.Fatalf("submitted %q with modules %q, want held with the gate's digest: %+v",
					held.Status, held.ModulesDigest(), held.DryRunScans)
			}
			if test.Publish {
				reg.publish("drifts", map[string]string{"main.tf": "output \"o\" {\n  value = " +
					"\"two\"\n}\n"})
			}
			if test.Forget {
				if err := os.RemoveAll(filepath.Join(files, moduleKeepDir)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.Approve(ctx, held.ID, decider("approver-pat", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			final := waitTerminal(t, store, held.ID)
			if final.Status != test.WantStatus || !strings.Contains(final.Error, test.WantError) {
				t.Errorf("run ended %q with %q, want %q saying %q", final.Status, final.Error,
					test.WantStatus, test.WantError)
			}
			plan.mu.Lock()
			defer plan.mu.Unlock()
			if plan.seen != test.WantSeen {
				t.Errorf("the plan used %q, want %q", plan.seen, test.WantSeen)
			}
			if test.WantSeen != "" && !plan.installed {
				t.Error("init was not told the modules are in place, so it could resolve them again")
			}
		})
	}
}

// TestAnApprovalBindsTheModulesTheGateRead covers the approval binding. The digest of the modules
// the gate read is part of the spec an approver decides on, so a run whose recorded digest changes
// after the decision no longer matches what was approved.
func TestAnApprovalBindsTheModulesTheGateRead(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run_bind", Tool: run.ToolTerraform, Command: "infra", DryRun: true,
		DryRunScans: []run.DryRunScan{{Tool: run.ToolTerraform, Scanner: "terraform-external",
			Version: 1, Classification: run.DryRunChangeFree,
			Fetch: &run.ModuleFetch{Command: "terraform get", ModulesDigest: "sha256:" +
				strings.Repeat("a", 64)}}}}
	before, err := outcome.SpecBinding(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := outcome.Spec(r)
	if err != nil || !strings.Contains(string(body), `"modules_digests":["sha256:aaaa`) {
		t.Errorf("spec = %s, %v, want the modules digest in it", body, err)
	}
	r.DryRunScans[0].Fetch.ModulesDigest = "sha256:" + strings.Repeat("b", 64)
	after, err := outcome.SpecBinding(r)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("the binding did not move when the modules the gate read changed")
	}
}

// TestARunThatCannotShowItsModulesIsRefused covers the runs pinning refuses before anything runs:
// one whose kept copy is gone on an executor that cannot download modules to check them, and one
// whose own environment moves the tool's data directory away from where the gate read the modules.
// A run the gate downloaded nothing for is left alone.
func TestARunThatCannotShowItsModulesIsRefused(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("c", 64)
	tests := []struct {
		// Digest is the modules digest the run's scan records, empty for none.
		Digest string
		// Env is the run's environment.
		Env []string
		// WantError is a fragment of the refusal, empty when the run goes ahead.
		WantError string
	}{{ // Test 0: No kept copy, and an executor that cannot download.
		Digest: digest, WantError: "cannot download them to check them",
	}, { // Test 1: A data directory the gate did not read from.
		Digest: digest, Env: []string{"TF_DATA_DIR=/var/tf"}, WantError: "sets TF_DATA_DIR",
	}, { // Test 2: Nothing downloaded, nothing pinned.
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor())
			t.Cleanup(d.Close)
			r := &run.Run{ID: "run_pin", Tool: run.ToolTerraform, Command: ".", DryRun: true}
			if test.Digest != "" {
				r.DryRunScans = []run.DryRunScan{{Tool: run.ToolTerraform,
					Fetch: &run.ModuleFetch{Command: "terraform get", ModulesDigest: test.Digest}}}
			}
			spec := roundhouse.Spec{Tool: run.ToolTerraform, Command: ".", Dir: t.TempDir(),
				Env: test.Env}
			err := d.pinModules(context.Background(), r, &spec, &masker{}, io.Discard)
			if test.WantError == "" {
				if err != nil || spec.ModulesInstalled {
					t.Errorf("pinModules() = %v with installed %v, want the run left alone", err,
						spec.ModulesInstalled)
				}
				return
			}
			if !errors.Is(err, errModulesRefused) || !strings.Contains(err.Error(), test.WantError) {
				t.Errorf("pinModules() = %v, want a refusal saying %q", err, test.WantError)
			}
		})
	}
}

// TestTheGateDownloadsEveryModuleAfresh covers the tree the gate keeps for the run when a working
// directory already holds some of its modules from an earlier init. The gate reads only the module
// files it scans, so a copy of what an earlier init left would keep a module without its other
// files, and the run would get that partial tree. The gate leaves the earlier install out and
// downloads every module afresh, so the tree it keeps is whole.
func TestTheGateDownloadsEveryModuleAfresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := newTestRegistry(t, map[string]registryModule{
		"clean": {Files: map[string]string{"main.tf": "output \"o\" {\n  value = 1\n}\n",
			"files/template.txt": "a file the module reads"}},
		"other": {Files: map[string]string{"main.tf": "output \"p\" {\n  value = 2\n}\n"}},
	})
	creds, sealer := cliCredential(t, reg.cliConfig(t))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.tf": fmt.Sprintf("module \"a\" {\n  source  = \"%s/acme/clean/null\"\n"+
			"  version = \"1.0.0\"\n}\nmodule \"b\" {\n  source  = \"%s/acme/other/null\"\n"+
			"  version = \"1.0.0\"\n}\n", registryHost, registryHost),
		".terraform/modules/modules.json": `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
			`{"Key":"a","Source":"` + registryHost + `/acme/clean/null","Version":"1.0.0",` +
			`"Dir":".terraform/modules/a"}]}`,
		".terraform/modules/a/main.tf": "output \"o\" {\n  value = 1\n}\n",
	})
	files := t.TempDir()
	d := New(run.NewMemStore(), newFetchingRunner(t), zap.NewNop(),
		WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))),
		WithCredentials(creds, sealer), WithRunFilesRoot(files), WithNoJanitor())
	t.Cleanup(d.Close)
	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true), run.WithCredentialIDs([]string{"cred_tf"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	digest := got.ModulesDigest()
	if digest == "" {
		t.Fatalf("scans = %+v, want the gate's download pinned", got.DryRunScans)
	}
	workdir := t.TempDir()
	if ok, err := d.modules.restore(digest, workdir); !ok || err != nil {
		t.Fatalf("restore() = %v, %v, want the kept tree", ok, err)
	}
	kept := filepath.Join(workdir, ".terraform", "modules", "a", "files", "template.txt")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the kept tree holds the earlier install's partial copy of module a: %v", err)
	}
}

// TestModuleKeepIsWhatTheOptionSays pins that a dispatcher keeps the module trees its gate
// downloads for as long, and within as much space, as it was configured to, and that leaving either
// unset keeps the default, so an install that sets neither behaves as it did before they existed.
func TestModuleKeepIsWhatTheOptionSays(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// KeepFor and MaxBytes are what WithModuleKeep is given.
		KeepFor  time.Duration
		MaxBytes int64
		// WantKeepFor and WantMaxBytes are what the store holds.
		WantKeepFor  time.Duration
		WantMaxBytes int64
	}{{ // Test 0: Nothing set keeps the defaults.
		WantKeepFor: DefaultModuleKeepFor, WantMaxBytes: DefaultModuleKeepMaxBytes,
	}, { // Test 1: Both set.
		KeepFor: time.Hour, MaxBytes: 64 << 20, WantKeepFor: time.Hour, WantMaxBytes: 64 << 20,
	}, { // Test 2: Only the time set keeps the default size.
		KeepFor: 30 * 24 * time.Hour, WantKeepFor: 30 * 24 * time.Hour,
		WantMaxBytes: DefaultModuleKeepMaxBytes,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor(),
				WithRunFilesRoot(t.TempDir()), WithModuleKeep(test.KeepFor, test.MaxBytes))
			t.Cleanup(d.Close)
			got := [2]int64{int64(d.modules.ttl), d.modules.maxBytes}
			want := [2]int64{int64(test.WantKeepFor), test.WantMaxBytes}
			if got != want {
				t.Errorf("kept for %v within %d bytes, want %v within %d", d.modules.ttl,
					d.modules.maxBytes, test.WantKeepFor, test.WantMaxBytes)
			}
		})
	}
}
