package runfiles

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

const (
	// helperRootEnv names the root a re-executed test binary works under before it waits to be
	// killed. Its presence is what turns the binary into the helper.
	helperRootEnv = "SWITCHTENDER_RUNFILES_CRASH_ROOT"
	// helperModeEnv says what the helper does under the root: own, stage, consume, many, or sweeplock.
	helperModeEnv = "SWITCHTENDER_RUNFILES_HELPER_MODE"
)

// TestMain lets the test binary double as a process that owns run directories and then dies without
// cleaning up, which is the only honest way to show what a crash leaves behind.
func TestMain(m *testing.M) {
	if root := os.Getenv(helperRootEnv); root != "" {
		if err := runHelper(root, os.Getenv(helperModeEnv)); err != nil {
			fmt.Println("ERR " + err.Error())
			os.Exit(1)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHelper does the helper's work under root and reports it on standard output, one line, before
// the caller sleeps until it is killed.
//
//   - own creates a run directory, writes a credential into it, and prints the credential's path.
//   - stage creates one and is part way through writing a credential when it prints its path.
//   - consume writes a credential, starts a child that keeps reading it, and prints the path and
//     the child's process id. The child outlives the helper the way a tool outlives a killed
//     worker.
//   - many creates three run directories and prints their paths separated by spaces.
//   - sweeplock takes the root's sweep lock, as a process in the middle of a pass does, and prints
//     held.
func runHelper(root, mode string) error {
	switch mode {
	case "", "own":
		d, err := Create(root, "crash")
		if err != nil {
			return err
		}
		p, err := d.WriteFile("cred-*", "kubeconfig-secret")
		if err != nil {
			return err
		}
		fmt.Println(p)
	case "stage":
		d, err := Create(root, "stage")
		if err != nil {
			return err
		}
		f, err := d.CreateTemp("cred-*")
		if err != nil {
			return err
		}
		if _, err := f.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\nhalf"); err != nil {
			return err
		}
		fmt.Println(f.Name())
	case "consume":
		d, err := Create(root, "consume")
		if err != nil {
			return err
		}
		p, err := d.WriteFile("cred-*", "vault-password")
		if err != nil {
			return err
		}
		reader := exec.Command("sh", "-c", `while :; do cat "$1" >/dev/null 2>&1; sleep 0.05; done`,
			"sh", p)
		if err := reader.Start(); err != nil {
			return err
		}
		fmt.Println(p + " " + strconv.Itoa(reader.Process.Pid))
	case "many":
		var paths []string
		for i := range 3 {
			d, err := Create(root, "worker-"+strconv.Itoa(i))
			if err != nil {
				return err
			}
			paths = append(paths, d.Path())
		}
		fmt.Println(strings.Join(paths, " "))
	case "sweeplock":
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(root, sweepLockName), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		if held, err := lockFile(f, true); err != nil || !held {
			return fmt.Errorf("take the sweep lock: %v", err)
		}
		fmt.Println("held")
	default:
		return fmt.Errorf("unknown helper mode %q", mode)
	}
	return nil
}

// TestCreateIsPrivate proves the permissions a run's secrets are written under: the root and the run
// directory 0700, each file 0600, the content intact, and nothing left after Remove.
func TestCreateIsPrivate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX modes; its temp directory is private by access list")
	}
	root := filepath.Join(t.TempDir(), "root")
	d, err := Create(root, "run_abc/../x")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	p, err := d.WriteFile("cred-*", "s3cret")
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if filepath.Dir(p) != d.Path() || filepath.Dir(d.Path()) != root {
		t.Fatalf("file %s is not inside run directory %s under %s", p, d.Path(), root)
	}
	if strings.Contains(filepath.Base(d.Path()), "..") || strings.Contains(filepath.Base(d.Path()), "/") {
		t.Errorf("label was not sanitized: %s", d.Path())
	}
	modes := map[string]fs.FileMode{}
	for name, path := range map[string]string{
		"root": root, "dir": d.Path(), "file": p, "beat": filepath.Join(d.Path(), beatName),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", name, err)
		}
		modes[name] = info.Mode().Perm()
	}
	want := map[string]fs.FileMode{"root": 0o700, "dir": 0o700, "file": 0o600, "beat": 0o600}
	if diff := cmp.Diff(want, modes); diff != "" {
		t.Errorf("modes mismatch (-want +got):\n%s", diff)
	}
	body, err := os.ReadFile(p)
	if err != nil || string(body) != "s3cret" {
		t.Errorf("ReadFile() = %q, %v, want the content", body, err)
	}
	if err := d.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := d.Remove(); err != nil {
		t.Errorf("second Remove() error = %v, want none", err)
	}
	if _, err := os.Stat(d.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run directory survived Remove: %v", err)
	}
}

// TestCreateRefusesAnUnsafeRoot proves a root another account could control is refused rather than
// written into.
func TestCreateRefusesAnUnsafeRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		Root     string
		WantMode fs.FileMode
		Want     error
	}{{ // Test 0: A symbolic link is refused, since its target can be swapped.
		Root: link, Want: ErrUnsafeRoot,
	}, { // Test 1: A root that is a file is refused.
		Root: file, Want: ErrUnsafeRoot,
	}, { // Test 2: A root of this account opened too wide is closed to 0700 and used.
		Root: loose, WantMode: 0o700, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d, err := Create(test.Root, "r")
			if !errors.Is(err, test.Want) {
				t.Fatalf("Create() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			defer func() { _ = d.Remove() }()
			info, err := os.Stat(test.Root)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != test.WantMode {
				t.Errorf("root mode = %v, want %v", info.Mode().Perm(), test.WantMode)
			}
		})
	}
}

// TestHeartbeatCountsOnATimer proves the owner advances its directory's counter on a timer, with no
// activity at all in the run, and that Remove stops it before the directory goes, so nothing writes
// into a directory that is being deleted.
func TestHeartbeatCountsOnATimer(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	// A new directory's counter starts at zero. It is read from a directory whose timer cannot fire
	// during the test: with a millisecond timer, a loaded machine ticked it before the first read.
	still, err := create(root, "still", time.Hour)
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(still.Path(), beatName)); err != nil {
		t.Fatalf("read the counter: %v", err)
	} else if !bytes.Equal(got, encodeBeat(0)) {
		t.Fatalf("a new directory's counter = %q, want %q", got, encodeBeat(0))
	}
	if err := still.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	d, err := create(root, "beat", time.Millisecond)
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}
	path := filepath.Join(d.Path(), beatName)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		now, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the counter: %v", err)
		}
		if !bytes.Equal(now, first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the counter never moved while its owner held the directory")
		}
		time.Sleep(time.Millisecond)
	}
	if err := d.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(d.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run directory survived Remove: %v", err)
	}
}

// TestRemoveClearsWhatAToolMadeReadOnly is cleanup failure handling. A tool can leave a directory
// it made read-only in a run directory, as Go's module cache does, and a plain removal then fails
// and leaves the run's secrets on disk. Remove makes the tree writable and tries again.
func TestRemoveClearsWhatAToolMadeReadOnly(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions that bind the account removing them")
	}
	root := filepath.Join(t.TempDir(), "root")
	d, err := Create(root, "readonly")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	cache := filepath.Join(d.Path(), "tools", "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "token"), []byte("cached-token"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0o500); err != nil {
		t.Fatal(err)
	}
	// The negative control: a plain removal of the same shape of tree fails.
	control := filepath.Join(t.TempDir(), "control")
	if err := os.MkdirAll(filepath.Join(control, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "cache", "token"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(control, "cache"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(control); err == nil {
		t.Fatal("a plain removal of a read-only tree succeeded, so this proves nothing")
	}
	_ = os.Chmod(filepath.Join(control, "cache"), 0o700)

	if err := d.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(d.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run directory survived Remove: %v", err)
	}
}

// TestRemoveLeavesWhatALinkPointsAt proves a symbolic link planted in a run directory is removed as
// a link: the file it points at, outside the run directory, keeps its content and its mode.
func TestRemoveLeavesWhatALinkPointsAt(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep me"), 0o640); err != nil {
		t.Fatal(err)
	}
	d, err := Create(filepath.Join(t.TempDir(), "root"), "link")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(d.Path(), "planted")); err != nil {
		t.Fatal(err)
	}
	// A read-only directory makes the first removal fail, so the permission repair runs too.
	ro := filepath.Join(d.Path(), "ro")
	if err := os.Mkdir(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ro, "planted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	body, err := os.ReadFile(outside)
	if err != nil || string(body) != "keep me" {
		t.Fatalf("the file the link pointed at = %q, %v, want it untouched", body, err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("the file the link pointed at is mode %v, want 0640 left alone", info.Mode().Perm())
	}
}
