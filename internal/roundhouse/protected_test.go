//go:build unix

package roundhouse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// protectedFixture is one server's layout on disk for the mount guard tests.
type protectedFixture struct {
	// DB is the database's directory, holding the database, the first admin token, and a backup.
	DB string
	// Home is the server account's home directory.
	Home string
	// Tmp is the temporary directory.
	Tmp string
	// RunFiles is the run-files root.
	RunFiles string
	// Projects is the project cache root.
	Projects string
	// Runner is a container runner with the fixture's paths registered.
	Runner *containerRunner
	// Plugin is the runner's callback plugin directory.
	Plugin string
}

// newProtectedFixture lays out a server's files under temporary directories and points HOME and
// TMPDIR at two of them, so the home and temporary directory rows judge directories the test owns.
func newProtectedFixture(t *testing.T) protectedFixture {
	t.Helper()
	base := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{base}, parts...)...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(path string) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := protectedFixture{
		DB: mk("db"), Home: mk("home"), Tmp: mk("tmp"), RunFiles: mk("runfiles"), Projects: mk("projects"),
	}
	for _, name := range []string{"switchtender.db", "switchtender.db-wal", initialTokenName, "backup.db",
		"site.yml", "producer-key.json"} {
		write(filepath.Join(f.DB, name))
	}
	write(filepath.Join(mk("db", "infra"), "site.yml"))
	write(filepath.Join(mk("tmp", "infra"), "site.yml"))
	write(filepath.Join(f.Tmp, "site.yml"))
	write(filepath.Join(f.Home, "site.yml"))
	mk("home", "infra")
	mk("home", ".ssh")
	write(filepath.Join(mk("runfiles", "run-1"), "vault"))
	write(filepath.Join(f.RunFiles, "run-1", "env"))
	write(filepath.Join(mk("runfiles", "run-2"), "site.yml"))
	write(filepath.Join(f.RunFiles, "run-2", "vault"))
	write(filepath.Join(mk("projects", "repo"), "site.yml"))
	write(filepath.Join(f.Projects, "repo", "hosts"))
	t.Setenv("HOME", f.Home)
	t.Setenv("TMPDIR", f.Tmp)
	f.Runner = newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	f.Runner.protected = &ProtectedPaths{
		DatabaseDir: f.DB, IdentityDir: f.DB, License: filepath.Join(f.DB, "switchtender-license.json"),
		AnsibleRoot: mk("ansible"), RunFiles: f.RunFiles, ProjectCache: f.Projects,
	}
	dir, err := f.Runner.plugin.create()
	if err != nil {
		t.Fatal(err)
	}
	f.Plugin = dir
	return f
}

// initialTokenName is the file the first admin token is written to beside the database.
const initialTokenName = "initial-admin-token"

// protectedRunArgs builds spec's container plan and its docker arguments with envFile mounted, the
// way a run does.
func protectedRunArgs(c *containerRunner, spec Spec, envFile string) ([]string, error) {
	// The guard is judged for an admin in person, whose named paths may reach an image at all.
	spec.PathsByAdmin = true
	plan, cleanup, err := buildContainerPlan(spec)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	return c.runArgs(spec, plan, "ym-test", envFile)
}

// TestProtectedPathsGuardEveryImageMount pins that no image run mounts the server's own files. The
// first row is the proof that opened the unit: an admin's Terraform run naming the database's
// directory got a read-write mount of it.
func TestProtectedPathsGuardEveryImageMount(t *testing.T) {
	f := newProtectedFixture(t)
	other := t.TempDir()
	link := filepath.Join(other, "link")
	if err := os.Symlink(f.DB, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tf := func(cmd string) Spec { return Spec{Tool: run.ToolTerraform, Command: cmd, Image: "x"} }
	pb := func(playbook, inventory string) Spec {
		return Spec{Tool: run.ToolAnsible, Playbook: playbook, Inventory: inventory, Image: "x"}
	}
	tests := []struct {
		Name    string
		Spec    Spec
		Env     string
		Chdir   string
		Want    error
		WantMsg string
	}{{ // Test 0: The database's directory as a Terraform command.
		Name: "terraform db dir", Spec: tf(f.DB), Want: ErrForbiddenMount, WantMsg: "this server's database",
	}, { // Test 1: The parent of the database's directory.
		Name: "terraform db parent", Spec: tf(f.DB + "/.."), Want: ErrForbiddenMount, WantMsg: "holds",
	}, { // Test 2: A symlink to the database's directory.
		Name: "terraform db link", Spec: tf(link), Want: ErrForbiddenMount, WantMsg: "this server's database",
	}, { // Test 3: The working directory, standing in the database's directory.
		Name: "terraform dot", Spec: tf("."), Chdir: f.DB, Want: ErrForbiddenMount,
		WantMsg: "this server's database",
	}, { // Test 4: A playbook beside the database.
		Name: "playbook beside db", Spec: pb(filepath.Join(f.DB, "site.yml"), ""), Want: ErrForbiddenMount,
		WantMsg: "Move the database with --db",
	}, { // Test 5: The first admin token as an inventory.
		Name: "inventory admin token",
		Spec: pb(filepath.Join(f.DB, "infra", "site.yml"), filepath.Join(f.DB, initialTokenName)),
		Want: ErrForbiddenMount, WantMsg: "sits beside this server's database",
	}, { // Test 6: A backup beside the database as an inventory.
		Name: "inventory backup",
		Spec: pb(filepath.Join(f.DB, "infra", "site.yml"), filepath.Join(f.DB, "backup.db")),
		Want: ErrForbiddenMount, WantMsg: "sits beside",
	}, { // Test 7: A playbook directly in the home directory.
		Name: "playbook in home", Spec: pb(filepath.Join(f.Home, "site.yml"), ""), Want: ErrForbiddenMount,
		WantMsg: "home directory",
	}, { // Test 8: A playbook directly in the temporary directory.
		Name: "playbook in tmp", Spec: pb(filepath.Join(f.Tmp, "site.yml"), ""), Want: ErrForbiddenMount,
		WantMsg: "Name a directory inside it.",
	}, { // Test 9: A playbook directly in /var/lib.
		Name: "playbook in var lib", Spec: pb("/var/lib/x.yml", ""), Want: ErrForbiddenMount,
		WantMsg: "/var/lib/docker",
	}, { // Test 10: A playbook directly in /tmp.
		Name: "playbook in slash tmp", Spec: pb("/tmp/x.yml", ""), Want: ErrForbiddenMount,
	}, { // Test 11: The run-files root as a Terraform directory.
		Name: "terraform run files", Spec: tf(f.RunFiles), Want: ErrForbiddenMount,
		WantMsg: "this server's run files",
	}, { // Test 12: The callback plugin directory as a Terraform directory.
		Name: "terraform plugin", Spec: tf(f.Plugin), Want: ErrForbiddenMount,
		WantMsg: "this server's callback plugin",
	}, { // Test 13: The project cache root as a Terraform directory.
		Name: "terraform project cache", Spec: tf(f.Projects), Want: ErrForbiddenMount,
		WantMsg: "this server's project checkouts",
	}, { // Test 14: The home directory as a Terraform directory.
		Name: "terraform home", Spec: tf(f.Home), Want: ErrForbiddenMount, WantMsg: "home directory",
	}, { // Test 15: A subdirectory of the database's directory.
		Name: "playbook below db", Spec: pb(filepath.Join(f.DB, "infra", "site.yml"), ""),
	}, { // Test 16: A subdirectory of the temporary directory.
		Name: "playbook below tmp", Spec: pb(filepath.Join(f.Tmp, "infra", "site.yml"), ""),
	}, { // Test 17: A subdirectory of the home directory.
		Name: "terraform below home", Spec: tf(filepath.Join(f.Home, "infra")),
	}, { // Test 18: A staged vault password, the run's own env file, and the plugin mount.
		Name: "staged files",
		Spec: Spec{Tool: run.ToolAnsible, Playbook: filepath.Join(f.DB, "infra", "site.yml"), Image: "x",
			RunFilesRoot: f.RunFiles, EventsPath: filepath.Join(f.RunFiles, "run-1", "events"),
			VaultPasswords: []VaultPassword{{Path: filepath.Join(f.RunFiles, "run-1", "vault")}}},
		Env: filepath.Join(f.RunFiles, "run-1", "env"),
	}, { // Test 19: A project checkout under the project cache root.
		Name: "project checkout",
		Spec: Spec{Tool: run.ToolAnsible, Dir: filepath.Join(f.Projects, "repo"), Image: "x",
			Playbook:  filepath.Join(f.Projects, "repo", "site.yml"),
			Inventory: filepath.Join(f.Projects, "repo", "hosts")},
	}, { // Test 20: A Terraform directory inside a project checkout.
		Name: "terraform in project", Spec: Spec{Tool: run.ToolTerraform, Dir: filepath.Join(f.Projects, "repo"),
			Command: ".", Image: "x"},
	}, { // Test 21: The .ssh directory's parent is the home directory, refused for holding it.
		Name: "ssh parent", Spec: tf(filepath.Join(f.Home, "infra", "..")), Want: ErrForbiddenMount,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			if test.Chdir != "" {
				t.Chdir(test.Chdir)
			}
			args, err := protectedRunArgs(f.Runner, test.Spec, test.Env)
			if !errors.Is(err, test.Want) {
				t.Fatalf("runArgs() error = %v, want %v (args %q)", err, test.Want, args)
			}
			if test.WantMsg != "" && !strings.Contains(err.Error(), test.WantMsg) {
				t.Errorf("runArgs() error = %q, want it to say %q", err, test.WantMsg)
			}
		})
	}
}

// TestNamedPathGetsNoPrivatePass pins that a path the run named is judged against the whole set,
// with no pass inside the run-files root, while a file the dispatcher staged beside it passes.
// Another run's directory sits under that root, so the pass let one run mount another's files.
func TestNamedPathGetsNoPrivatePass(t *testing.T) {
	f := newProtectedFixture(t)
	named := Spec{Tool: run.ToolAnsible, Playbook: filepath.Join(f.RunFiles, "run-2", "site.yml"),
		Image: "x", RunFilesRoot: f.RunFiles}
	if _, err := protectedRunArgs(f.Runner, named, ""); !errors.Is(err, ErrForbiddenMount) {
		t.Errorf("playbook in another run's directory: error = %v, want ErrForbiddenMount", err)
	}
	staged := Spec{Tool: run.ToolAnsible, Playbook: filepath.Join(f.DB, "infra", "site.yml"), Image: "x",
		RunFilesRoot:   f.RunFiles,
		VaultPasswords: []VaultPassword{{Path: filepath.Join(f.RunFiles, "run-2", "vault")}}}
	if _, err := protectedRunArgs(f.Runner, staged, ""); err != nil {
		t.Errorf("staged vault password beside it: error = %v, want none", err)
	}
	inv := Spec{Tool: run.ToolAnsible, Playbook: filepath.Join(f.DB, "infra", "site.yml"), Image: "x",
		RunFilesRoot: f.RunFiles, Inventory: filepath.Join(f.RunFiles, "run-2", "vault")}
	if _, err := protectedRunArgs(f.Runner, inv, ""); !errors.Is(err, ErrForbiddenMount) {
		t.Errorf("named inventory in another run's directory: error = %v, want ErrForbiddenMount", err)
	}
	inv.InventoryStaged = true
	if _, err := protectedRunArgs(f.Runner, inv, ""); err != nil {
		t.Errorf("stored inventory the dispatcher staged: error = %v, want none", err)
	}
}

// TestProtectedPathsMessageNamesTheFix pins the refusal's words: the server path that caused it and
// what to do.
func TestProtectedPathsMessageNamesTheFix(t *testing.T) {
	f := newProtectedFixture(t)
	_, err := protectedRunArgs(f.Runner, Spec{Tool: run.ToolTerraform, Command: f.DB, Image: "x"}, "")
	want := f.DB + " holds this server's database (" + f.DB + "). Move the database with --db to its " +
		"own directory, or name the subdirectory holding the configuration."
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to hold %q", err, want)
	}
}

// TestNamedMountFactDecidesAnImageRun pins the caller rule where the image is known: a path the run
// named reaches an image only when an admin in person named it, and a missing fact refuses with the
// path, the image, and what to do. A path inside a project is not named, so it needs no fact.
func TestNamedMountFactDecidesAnImageRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	named := Spec{Tool: run.ToolTerraform, Command: dir, Image: "registry.example/tf:1"}
	_, cleanup, err := buildContainerPlan(named)
	cleanup()
	if !errors.Is(err, ErrNamedPathBelowAdmin) {
		t.Fatalf("named path without the fact: error = %v, want ErrNamedPathBelowAdmin", err)
	}
	for _, want := range []string{dir, "registry.example/tf:1", "Add the repository as a project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not hold %q", err, want)
		}
	}
	named.PathsByAdmin = true
	plan, cleanup, err := buildContainerPlan(named)
	cleanup()
	if err != nil {
		t.Fatalf("named path with the fact: error = %v", err)
	}
	if len(plan.mounts) == 0 || plan.mounts[0].path != dir {
		t.Errorf("mounts = %+v, want the named directory", plan.mounts)
	}
	inProject := Spec{Tool: run.ToolAnsible, Dir: dir, Playbook: filepath.Join(dir, "site.yml"), Image: "x"}
	_, cleanup, err = buildContainerPlan(inProject)
	cleanup()
	if err != nil {
		t.Errorf("a project's playbook without the fact: error = %v, want none", err)
	}
	_, cleanup, err = buildModulesPlan(Spec{Tool: run.ToolTerraform, Command: dir, Image: "x"})
	cleanup()
	if !errors.Is(err, ErrNamedPathBelowAdmin) {
		t.Errorf("module get of a named directory without the fact: error = %v, want ErrNamedPathBelowAdmin", err)
	}
}
