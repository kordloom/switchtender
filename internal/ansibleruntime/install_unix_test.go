//go:build !windows

package ansibleruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestInstallBuildsVerifiesAndMarks pins a fresh install: one venv, one pip call in hash-checking
// mode from wheels only, the lock copied in verbatim, the marker written, and the release made
// current.
func TestInstallBuildsVerifiesAndMarks(t *testing.T) {
	t.Parallel()
	lock, err := LockFor("2.21")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	py, logPath := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	root := filepath.Join(t.TempDir(), "ansible")
	res, err := Install(context.Background(), InstallOptions{Root: root, Version: "2.21", Python: py})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	dir := filepath.Join(root, envName(lock))
	got := struct {
		Already  bool
		Release  string
		Python   string
		Dir      string
		Lock     string
		Current  bool
		PipCalls int
	}{res.AlreadyInstalled, res.Runtime.Release, res.Runtime.Python, res.Runtime.Dir,
		res.Runtime.LockSHA256, res.Runtime.Current, countPrefix(calls(t, logPath), "pip ")}
	want := struct {
		Already  bool
		Release  string
		Python   string
		Dir      string
		Lock     string
		Current  bool
		PipCalls int
	}{false, lock.Release, "3.12.3", dir, lock.SHA256(), true, 1}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Install() mismatch (-want +got):\n%s", diff)
	}
	pip := calls(t, logPath)[1]
	for _, arg := range []string{"--require-hashes", "--only-binary=:all:", "--no-cache-dir",
		"-r " + filepath.Join(dir, lockCopyName)} {
		if !strings.Contains(pip, arg) {
			t.Errorf("pip was run as %q, without %q", pip, arg)
		}
	}
	copied, err := os.ReadFile(filepath.Join(dir, lockCopyName))
	if err != nil || string(copied) != string(lock.Content) {
		t.Errorf("the lock pip read is not the embedded lock (err %v)", err)
	}
	cur, err := Current(root)
	if err != nil || cur == nil || cur.Release != lock.Release {
		t.Errorf("Current() = %+v, %v, want %s", cur, err, lock.Release)
	}
}

// TestInstallIsIdempotent pins that installing a release that is installed from the same lock and
// still verifies only verifies it, and that a later install of another release becomes current
// without touching the first.
func TestInstallIsIdempotent(t *testing.T) {
	t.Parallel()
	py, logPath := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	root := t.TempDir()
	ctx := context.Background()
	for i := range 2 {
		res, err := Install(ctx, InstallOptions{Root: root, Python: py})
		if err != nil {
			t.Fatalf("Install() %d error = %v", i, err)
		}
		if res.AlreadyInstalled != (i == 1) {
			t.Errorf("Install() %d AlreadyInstalled = %v, want %v", i, res.AlreadyInstalled, i == 1)
		}
	}
	if n := countPrefix(calls(t, logPath), "pip "); n != 1 {
		t.Errorf("pip ran %d times over two installs of one release, want 1", n)
	}
	if _, err := Install(ctx, InstallOptions{Root: root, Version: "2.20", Python: py}); err != nil {
		t.Fatalf("Install(2.20) error = %v", err)
	}
	list, err := List(root)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var got []string
	for _, rt := range list {
		got = append(got, fmt.Sprintf("%s current=%v", rt.Release, rt.Current))
	}
	newest := Releases()[len(Releases())-1]
	want := []string{newest + " current=false", "2.20.9 current=true"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
}

// TestInstallFailures pins every way an install fails: each one returns its error, leaves no
// environment behind, and leaves the runtime in use before still in use.
func TestInstallFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want        error
		WantInError []string
		Python      fakePythonOpts
		Version     string
	}{{ // Test 0: A file that does not match the lock's hash fails the install.
		Python: fakePythonOpts{Version: "3.12.3", HashMismatch: true},
		Want:   ErrHashMismatch, WantInError: []string{"nothing was installed"},
	}, { // Test 1: An install that reports another release fails verification.
		Python: fakePythonOpts{Version: "3.12.3", Reports: "2.99.0"},
		Want:   ErrVerify, WantInError: []string{"reported ansible-core 2.99.0"},
	}, { // Test 2: A Python below the release's range is refused, stating the range.
		Python: fakePythonOpts{Version: "3.9.6"}, Want: ErrPython,
		WantInError: []string{"runs on Python 3.12 to 3.14", "is Python 3.9.6", "--python"},
	}, { // Test 3: A Python above the release's range is refused.
		Python: fakePythonOpts{Version: "3.13.1"}, Version: "2.16", Want: ErrPython,
		WantInError: []string{"runs on Python 3.10 to 3.12", "is Python 3.13.1"},
	}, { // Test 4: A Python without venv support is refused, naming the package.
		Python: fakePythonOpts{Version: "3.12.3", NoVenv: true}, Want: ErrPython,
		WantInError: []string{"python3.12-venv"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			before := fakeRuntime(t, root, "2.19.13", true)
			lock, err := LockFor(test.Version)
			if err != nil {
				t.Fatalf("LockFor() error = %v", err)
			}
			dir := filepath.Join(root, envName(lock))
			py, _ := fakePython(t, test.Python)
			_, err = Install(context.Background(), InstallOptions{Root: root, Version: test.Version,
				Python: py})
			if !errors.Is(err, test.Want) {
				t.Fatalf("Install() error = %v, want %v", err, test.Want)
			}
			for _, w := range test.WantInError {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Install() error = %q, want it to say %q", err, w)
				}
			}
			_, statErr := os.Stat(filepath.Join(dir, markerName))
			if !os.IsNotExist(statErr) {
				t.Errorf("a failed install left a marker in %s", dir)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("a failed install left %s behind", dir)
			}
			cur, err := Current(root)
			if err != nil || cur == nil || cur.Dir != before {
				t.Errorf("after a failed install Current() = %+v, %v, want %s", cur, err, before)
			}
		})
	}
}

// TestInstallLeavesAForeignDirectoryAlone pins that a directory in the runtime's place that the
// installer did not make is never touched: the install builds beside it and succeeds.
func TestInstallLeavesAForeignDirectoryAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lock, err := LockFor("")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	foreign := filepath.Join(root, envName(lock))
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	py, _ := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	res, err := Install(context.Background(), InstallOptions{Root: root, Python: py})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if res.Runtime.Dir == foreign {
		t.Errorf("Install() built into the foreign directory %s", foreign)
	}
	if b, err := os.ReadFile(filepath.Join(foreign, "keep")); err != nil || string(b) != "x" {
		t.Errorf("the foreign directory was touched: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(foreign, markerName)); !os.IsNotExist(err) {
		t.Errorf("Install() wrote a marker into the foreign directory: %v", err)
	}
}

// TestInstallReplacesAnUnfinishedInstall pins that an install interrupted before its marker is
// treated as absent, never trusted, and built anew.
func TestInstallReplacesAnUnfinishedInstall(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lock, err := LockFor("")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	dir := fakeRuntime(t, root, lock.Release, false)
	if err := os.Remove(filepath.Join(dir, markerName)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pyvenv.cfg"), []byte("home = x\n"), 0o644); err != nil {
		t.Fatalf("write pyvenv.cfg: %v", err)
	}
	py, logPath := fakePython(t, fakePythonOpts{Version: "3.13.0"})
	res, err := Install(context.Background(), InstallOptions{Root: root, Python: py})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if res.AlreadyInstalled || countPrefix(calls(t, logPath), "pip ") != 1 {
		t.Errorf("an unfinished install was trusted: already %v, calls %q", res.AlreadyInstalled,
			calls(t, logPath))
	}
}

// TestPipArgs pins the pip command line: hash-checking mode and wheels only always, and an offline
// install reading only the named wheel directory.
func TestPipArgs(t *testing.T) {
	t.Parallel()
	base := []string{"-I", "-m", "pip", "install", "--require-hashes", "--only-binary=:all:",
		"--no-cache-dir", "--no-input", "--disable-pip-version-check", "-r", "/l.txt"}
	tests := []struct {
		WantArgs []string
		Wheels   string
		IndexURL string
	}{{ // Test 0: Online, from PyPI, the default index.
		WantArgs: base,
	}, { // Test 1: Offline, from a wheel directory and no index.
		Wheels:   "/w",
		WantArgs: append(slices.Clone(base), "--no-index", "--find-links", "/w"),
	}, { // Test 2: From a mirror named on the command line.
		IndexURL: "https://mirror.example/simple",
		WantArgs: append(slices.Clone(base), "--index-url", "https://mirror.example/simple"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := pipArgs("/l.txt", test.Wheels, test.IndexURL)
			if diff := cmp.Diff(test.WantArgs, got); diff != "" {
				t.Errorf("pipArgs() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRemove pins removal: one release, with current moved to the newest left, a release that is
// not installed, and everything, which also removes the directory.
func TestRemove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want        error
		WantRemoved []string
		WantLeft    []string
		WantCurrent string
		Version     string
	}{{ // Test 0: Removing the current release moves current to the newest left.
		Version: "2.21.4", WantRemoved: []string{"2.21.4"}, WantLeft: []string{"2.20.9", "2.19.13"},
		WantCurrent: "2.20.9",
	}, { // Test 1: A minor version removes its release.
		Version: "2.19", WantRemoved: []string{"2.19.13"}, WantLeft: []string{"2.21.4", "2.20.9"},
		WantCurrent: "2.21.4",
	}, { // Test 2: A release that is not installed is an error and removes nothing.
		Version: "2.16", Want: ErrNotInstalled,
		WantLeft: []string{"2.21.4", "2.20.9", "2.19.13"}, WantCurrent: "2.21.4",
	}, { // Test 3: No version removes every runtime and the directory.
		WantRemoved: []string{"2.19.13", "2.20.9", "2.21.4"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "ansible")
			for _, r := range []string{"2.19.13", "2.20.9", "2.21.4"} {
				fakeRuntime(t, root, r, r == "2.21.4")
			}
			removed, err := Remove(root, test.Version)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Remove() error = %v, want %v", err, test.Want)
			}
			var gotRemoved []string
			for _, d := range removed {
				gotRemoved = append(gotRemoved, envPattern.FindStringSubmatch(filepath.Base(d))[1])
			}
			slices.Sort(gotRemoved)
			list, err := List(root)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var left []string
			cur := ""
			for _, rt := range list {
				left = append(left, rt.Release)
				if rt.Current {
					cur = rt.Release
				}
			}
			got := struct {
				Removed, Left []string
				Current       string
			}{gotRemoved, left, cur}
			want := struct {
				Removed, Left []string
				Current       string
			}{test.WantRemoved, test.WantLeft, test.WantCurrent}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Remove() mismatch (-want +got):\n%s", diff)
			}
			if test.Version == "" {
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Errorf("removing everything left %s: %v", root, err)
				}
			}
		})
	}
}

// TestANewLockGetsItsOwnEnvironment pins what an install from a changed lock does to the release
// already in use: the new lock builds beside it, a failed build leaves the old one current and
// whole, and a verified one becomes current while the old one stays on disk for the runs using it.
func TestANewLockGetsItsOwnEnvironment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	oldLock, err := LockFor("")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	newLock, err := ParseLock(append(append([]byte{}, oldLock.Content...),
		[]byte("# regenerated\n")...))
	if err != nil {
		t.Fatalf("ParseLock() error = %v", err)
	}
	good, _ := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	broken, _ := fakePython(t, fakePythonOpts{Version: "3.12.3", Reports: "2.99.0"})
	if _, err := Install(ctx, InstallOptions{Root: root, Python: good, lock: oldLock}); err != nil {
		t.Fatalf("Install(old) error = %v", err)
	}
	oldDir := filepath.Join(root, envName(oldLock))
	_, err = Install(ctx, InstallOptions{Root: root, Python: broken, lock: newLock})
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("Install(new, broken) error = %v, want %v", err, ErrVerify)
	}
	if cur, err := Current(root); err != nil || cur == nil || cur.Dir != oldDir {
		t.Fatalf("after a failed rebuild Current() = %+v, %v, want %s", cur, err, oldDir)
	}
	if _, err := Install(ctx, InstallOptions{Root: root, Python: good, lock: newLock}); err != nil {
		t.Fatalf("Install(new) error = %v", err)
	}
	list, err := List(root)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var got []string
	for _, rt := range list {
		got = append(got, fmt.Sprintf("%s current=%v", filepath.Base(rt.Dir), rt.Current))
	}
	want := []string{envName(newLock) + " current=true", envName(oldLock) + " current=false"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
}
