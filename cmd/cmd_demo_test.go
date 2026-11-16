package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDemoPathsKeepIdentityBesideTheResolvedDatabase covers where the demo's producer identity comes
// from when --db is empty.
//
// The identity was loaded from the directory of the raw flag rather than the resolved database, and
// the directory of an empty path is the working directory. The default database path is a bare
// filename, so a default install keeps its production key in the directory it serves from: running
// the demo there published the production install identity, public key, and fingerprint on the
// demo's unauthenticated trust document, which is exposed to the internet by design. Run anywhere
// else it created a fresh private key in an unrelated directory.
func TestDemoPathsKeepIdentityBesideTheResolvedDatabase(t *testing.T) {
	original := demoDB
	t.Cleanup(func() { demoDB = original })

	// The working directory stands in for a production install's serve directory, holding the key
	// the demo must not reach for.
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	production, err := audit.LoadIdentity(".")
	if err != nil {
		t.Fatalf("LoadIdentity() for the production key error = %v", err)
	}

	// Test 0: An explicit --db keeps the identity beside that database.
	demoDB = filepath.Join(t.TempDir(), "demo.db")
	db, keyDir, err := demoPaths()
	if err != nil {
		t.Fatalf("demoPaths() error = %v", err)
	}
	if db != demoDB || keyDir != filepath.Dir(demoDB) {
		t.Errorf("demoPaths() = %q, %q, want %q beside %q", db, keyDir, demoDB, filepath.Dir(demoDB))
	}

	// Test 1: An empty --db resolves to a temporary database, and the identity follows it rather
	// than the working directory.
	demoDB = ""
	db, keyDir, err = demoPaths()
	if err != nil {
		t.Fatalf("demoPaths() with an empty --db error = %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(db) })
	if db == "" {
		t.Fatal("demoPaths() with an empty --db returned no database")
	}
	if keyDir != filepath.Dir(db) {
		t.Errorf("demoPaths() key directory = %q, want %q beside the resolved database %q",
			keyDir, filepath.Dir(db), db)
	}

	// Test 2: The identity the demo would publish is its own, not the one in the serve directory.
	demoID, err := audit.LoadIdentity(keyDir)
	if err != nil {
		t.Fatalf("LoadIdentity(%q) error = %v", keyDir, err)
	}
	if demoID.InstallID == production.InstallID || demoID.KeyID() == production.KeyID() {
		t.Errorf("the demo published install %q key %q, which is the production identity in the "+
			"working directory. That key and fingerprint reach the unauthenticated trust document",
			demoID.InstallID, demoID.KeyID())
	}
}

// TestDemoTakesTheProxyFlagsServeTakes covers the one build that is actually exposed to the public
// internet behind a reverse proxy, and had no way to be told so.
//
// Every per-client budget resolves the caller through clientAddr, which believes a forwarding header
// only from a proxy the operator named. The demo never had the flag, so trustedProxies was always
// empty and every visitor resolved to the proxy's own address: the 32-stream-per-caller cap became a
// cap of 32 across all visitors at once. The run at the center of the approvals story is held, so it
// is non-terminal, and each visitor who opens it and leaves the tab open holds a slot until they
// close it.
func TestDemoTakesTheProxyFlagsServeTakes(t *testing.T) {
	for _, name := range []string{"trusted-proxy", "client-ip-header"} {
		if demoCmd.Flags().Lookup(name) == nil {
			t.Errorf("demo has no --%s, so behind a proxy every visitor shares one per-client "+
				"budget and the cap stops describing a caller", name)
		}
		if serveCmd.Flags().Lookup(name) == nil {
			t.Errorf("serve has no --%s, which this test assumed as the reference", name)
		}
	}
}

// TestDemoSeedOnlyHelpSaysWhatTheResultIsFor covers the --seed-only help text.
//
// A database seeded with --seed-only is served by demo --no-seed and refused by serve, because
// seeded runs carry no approval seal and no adoption record. Without the sentence, the refusal from
// serve reads as a bug in the seeding rather than a boundary.
func TestDemoSeedOnlyHelpSaysWhatTheResultIsFor(t *testing.T) {
	flag := demoCmd.Flags().Lookup("seed-only")
	if flag == nil {
		t.Fatal("demo has no --seed-only")
	}
	want := "The result is served by demo --no-seed. serve refuses it on purpose: seeded runs " +
		"carry no approval seal and no adoption record."
	if !strings.Contains(flag.Usage, want) {
		t.Errorf("--seed-only help = %q, want it to contain %q", flag.Usage, want)
	}
}

// TestATemporaryDemoLeavesNothingBehind covers what a demo run without --db leaves in the temp
// directory when it stops.
//
// Every run left its database, the write-ahead log and shared-memory files beside it, and a tree of
// playbooks and Terraform, so a machine that ran the demo often held hundreds of them. The producer
// identity lives in the same directory and has to survive, since it is what the next run signs with.
func TestATemporaryDemoLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := filepath.Join(dir, "switchtender-demo-1.db")
	assets := filepath.Join(dir, "switchtender-demo-assets-1")
	identity := filepath.Join(dir, "switchtender-audit.key")
	for _, name := range []string{db, db + "-wal", db + "-shm", db + "-journal", identity} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(assets, "ansible"), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(assets, "ansible", "site.yml"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	removeDemoFiles(db, assets)

	for _, name := range []string{db, db + "-wal", db + "-shm", db + "-journal", assets} {
		if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there after the demo stopped (Stat error = %v)", filepath.Base(name), err)
		}
	}
	if _, err := os.Stat(identity); err != nil {
		t.Errorf("the producer identity beside the database was removed with it: %v", err)
	}
}

// TestOnlyATemporaryDemoRemovesItsFiles covers which demo runs clean up after themselves. A --db run
// was pointed at a database someone wants, and a --seed-only run exists to leave one behind.
func TestOnlyATemporaryDemoRemovesItsFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		DB       string
		SeedOnly bool
		WantOwns bool
	}{{ // Test 0: No --db is a temporary database the run made for itself.
		DB: "", SeedOnly: false, WantOwns: true,
	}, { // Test 1: --seed-only keeps the database it seeded.
		DB: "", SeedOnly: true, WantOwns: false,
	}, { // Test 2: --db keeps what it was pointed at.
		DB: "demo.db", SeedOnly: false, WantOwns: false,
	}, { // Test 3: --db with --seed-only keeps it too.
		DB: "demo.db", SeedOnly: true, WantOwns: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := ownsDemoFiles(test.DB, test.SeedOnly); got != test.WantOwns {
				t.Errorf("ownsDemoFiles(%q, %v) = %v, want %v", test.DB, test.SeedOnly, got, test.WantOwns)
			}
		})
	}
}

// TestDemoDispatcherHoldsARunFromALocalProject pins the demo's project wiring. Every run the demo
// holds comes from one of its seeded projects, which are git folders on the demo server, so its
// dispatcher syncs them and a held run from one is pinned to the project's commit. Without a syncer
// the demo refused every run that named a project, and a held run naming its playbook by path is
// refused by the rule, so the demo could hold nothing at all.
func TestDemoDispatcherHoldsARunFromALocalProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bundle, err := openBundle(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	p := dispatchtest.LocalProject(t, nil)
	p.SaveIn(t, bundle.Projects())
	disp, closeDisp, err := demoDispatcher(bundle, dispatchtest.OKRunner(), nil, live.NewHub(),
		time.Now, zap.NewNop())
	if err != nil {
		t.Fatalf("demoDispatcher() error = %v", err)
	}
	t.Cleanup(closeDisp)

	held, err := disp.Submit(ctx, "site.yml", "", run.WithProject(p.ID),
		run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() from the project error = %v", err)
	}
	if held.Status != run.StatusPendingApproval || held.PinnedCommit != p.Head {
		t.Errorf("held run = %s pinned to %q, want pending_approval pinned to %q", held.Status,
			held.PinnedCommit, p.Head)
	}
	if _, err := disp.Submit(ctx, "/srv/playbooks/site.yml", "",
		run.WithRequireApproval(true)); !errors.Is(err, dispatch.ErrHeldByPath) {
		t.Errorf("Submit() by path error = %v, want %v", err, dispatch.ErrHeldByPath)
	}
}

// newDemoAnsibleCommands writes stand-ins for the Ansible commands into dir. Nothing starts them:
// the tests ask only where the demo would start them from.
func newDemoAnsibleCommands(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"ansible", "ansible-playbook", "ansible-inventory"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// newDemoManagedAnsible writes a managed runtime in use under root, laid out as a finished install
// leaves it: an environment named for its release and the start of its lock's digest, the marker,
// and current naming it. It returns the directory holding the runtime's commands.
func newDemoManagedAnsible(t *testing.T, root string) string {
	t.Helper()
	env := "2.21.4-" + strings.Repeat("0", 12)
	bin := filepath.Join(root, env, "bin")
	newDemoAnsibleCommands(t, bin)
	marker := `{"ansible_core":"2.21.4","python":"3.12.3","lock_sha256":"` +
		strings.Repeat("0", 64) + `"}`
	if err := os.WriteFile(filepath.Join(root, env, "switchtender-runtime.json"), []byte(marker),
		0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "current"), []byte(env+"\n"), 0o644); err != nil {
		t.Fatalf("write current: %v", err)
	}
	return bin
}

// newDemoWithoutAnsible leaves the test with no Ansible anywhere the demo looks: no flag or
// environment variable choosing one, a PATH that holds nothing, and an empty working directory,
// which it returns. Everything is put back when the test ends.
func newDemoWithoutAnsible(t *testing.T) string {
	t.Helper()
	bin, dir, db := ansibleBinFlag, ansibleRuntimeDirFlag, demoDB
	t.Cleanup(func() { ansibleBinFlag, ansibleRuntimeDirFlag, demoDB = bin, dir, db })
	ansibleBinFlag, ansibleRuntimeDirFlag, demoDB = "", "", ""
	t.Setenv(ansibleruntime.RootEnv, "")
	t.Setenv(dbEnvVar, "")
	t.Setenv("PATH", t.TempDir())
	work := t.TempDir()
	t.Chdir(work)
	return work
}

// resolvedPath returns path with its symbolic links resolved, so two spellings of one temporary
// directory compare equal. An empty path, or one that does not exist, comes back as it was given.
func resolvedPath(path string) string {
	if path == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// TestDemoRunnerResolvesTheManagedAnsible covers where the demo's runner starts Ansible from when
// PATH holds none.
//
// The demo built its runner with no locator, so it read PATH alone, and `switchtender ansible
// install` puts nothing on PATH. A reader told to install the managed runtime ran the demo again
// and got the same failure. The runner now looks where that command writes with no flags: beside
// the default database in the working directory, beside the database SWITCHTENDER_DB names, or
// beside the demo's own --db. Deliberately not parallel: it changes PATH, the environment, and the
// working directory.
func TestDemoRunnerResolvesTheManagedAnsible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the managed runtime's ownership checks are not made on Windows")
	}
	tests := []struct {
		Want       error
		WantSource string
		Installed  bool
		DBFlag     bool
		DBEnv      bool
	}{{ // Test 0: A runtime beside the default database in the working directory is found.
		Installed: true, WantSource: ansibleruntime.SourceManaged,
	}, { // Test 1: With --db the runtime beside that database is found.
		Installed: true, DBFlag: true, WantSource: ansibleruntime.SourceManaged,
	}, { // Test 2: With SWITCHTENDER_DB the runtime beside that database is found.
		Installed: true, DBEnv: true, WantSource: ansibleruntime.SourceManaged,
	}, { // Test 3: With no runtime installed the runner falls to PATH, which holds none.
		WantSource: ansibleruntime.SourcePath, Want: ansibleruntime.ErrCommandMissing,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			data := newDemoWithoutAnsible(t)
			switch {
			case test.DBFlag:
				data = t.TempDir()
				demoDB = filepath.Join(data, "demo.db")
			case test.DBEnv:
				data = t.TempDir()
				t.Setenv(dbEnvVar, filepath.Join(data, "install.db"))
			}
			wantPath := ""
			if test.Installed {
				bin := newDemoManagedAnsible(t, filepath.Join(data, ansibleruntime.DirName))
				wantPath = resolvedPath(filepath.Join(bin, "ansible-playbook"))
			}
			runner := demoRunner(demoAnsibleLocator(demoCmd))
			reporter, ok := runner.(roundhouse.AnsibleCommandsReporter)
			if !ok {
				t.Fatal("the demo's runner does not report where its Ansible comes from")
			}
			cmds := reporter.AnsibleCommands()
			if diff := cmp.Diff(test.WantSource, cmds.Source); diff != "" {
				t.Errorf("AnsibleCommands().Source mismatch (-want +got):\n%s", diff)
			}
			got, err := cmds.Path("ansible-playbook")
			if !errors.Is(err, test.Want) {
				t.Fatalf("Path() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(wantPath, resolvedPath(got)); diff != "" {
				t.Errorf("Path() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDemoPreflightNamesTheAnsibleNeed covers what the demo says before it seeds on a machine
// where it cannot start Ansible.
//
// Seeding runs real playbooks. With no Ansible the demo made its database, started seeding, and
// several seconds later failed with a raw exec error that named no fix. It now stops before
// anything is made, with one line naming the need, the command that meets it, and the Python
// versions that command needs. A --no-seed run starts no playbook and is not stopped.
// Deliberately not parallel: it changes PATH, the environment, and the working directory.
func TestDemoPreflightNamesTheAnsibleNeed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the managed runtime's ownership checks are not made on Windows")
	}
	lock, err := ansibleruntime.LockFor("")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	python := "which needs Python " + lock.PythonMin + " to " + lock.PythonMax + ": "
	const plain = "or run `switchtender ansible install` in this folder, "
	tests := []struct {
		Want       error
		WantLine   []string
		WantAbsent string
		Installed  bool
		Broken     bool
		OnPath     bool
		DBFlag     bool
		NoSeed     bool
	}{{ // Test 0: No Ansible anywhere names the need, the command, and the Python it needs.
		Want: ansibleruntime.ErrCommandMissing,
		WantLine: []string{"demo needs Ansible, because seeding runs real playbooks: put " +
			"ansible-playbook and ansible-inventory on PATH, " + plain + python},
	}, { // Test 1: A --no-seed run with no Ansible is not stopped and prints nothing.
		NoSeed: true,
	}, { // Test 2: Ansible on PATH passes.
		OnPath: true,
	}, { // Test 3: A managed runtime in use passes with PATH holding none.
		Installed: true,
	}, { // Test 4: With --db the command names the directory beside that database.
		DBFlag: true, Want: ansibleruntime.ErrCommandMissing,
		WantLine:   []string{"or run `switchtender ansible install --dir ", "ansible`, " + python},
		WantAbsent: "in this folder",
	}, { // Test 5: A runtime in use that is not there gives its reason and no install advice.
		Broken: true, Want: ansibleruntime.ErrUnusable,
		WantLine:   []string{"demo needs Ansible, because seeding runs real playbooks: "},
		WantAbsent: "ansible install",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			data := newDemoWithoutAnsible(t)
			if test.DBFlag {
				data = t.TempDir()
				demoDB = filepath.Join(data, "demo.db")
			}
			root := filepath.Join(data, ansibleruntime.DirName)
			switch {
			case test.Installed:
				newDemoManagedAnsible(t, root)
			case test.Broken:
				newDemoManagedAnsible(t, root)
				missing := "2.21.4-" + strings.Repeat("1", 12)
				if err := os.WriteFile(filepath.Join(root, "current"), []byte(missing+"\n"),
					0o644); err != nil {
					t.Fatalf("write current: %v", err)
				}
			case test.OnPath:
				onPath := t.TempDir()
				newDemoAnsibleCommands(t, onPath)
				t.Setenv("PATH", onPath)
			}
			err := checkDemoAnsible(demoAnsibleLocator(demoCmd), demoDB, test.NoSeed)
			if !errors.Is(err, test.Want) {
				t.Fatalf("checkDemoAnsible() error = %v, want %v", err, test.Want)
			}
			if err == nil {
				return
			}
			line := err.Error()
			if strings.Contains(line, "\n") {
				t.Errorf("checkDemoAnsible() printed more than one line: %q", line)
			}
			for _, want := range test.WantLine {
				if !strings.Contains(line, want) {
					t.Errorf("checkDemoAnsible() = %q, want it to contain %q", line, want)
				}
			}
			if test.WantAbsent != "" && strings.Contains(line, test.WantAbsent) {
				t.Errorf("checkDemoAnsible() = %q, want it without %q", line, test.WantAbsent)
			}
		})
	}
}
