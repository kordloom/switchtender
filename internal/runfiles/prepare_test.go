package runfiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// localFS stands in for a known local filesystem.
func localFS(string) (filesystem, error) {
	return filesystem{Name: "ext4", Class: classLocal}, nil
}

// TestPrepareAcceptsAPrivateLocalRoot proves the real checks pass on this machine's temporary
// directory, which is where the default root lives, and that the probe leaves nothing behind.
func TestPrepareAcceptsAPrivateLocalRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	report, err := Prepare(root)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if report.Root != root || report.Filesystem == "" {
		t.Errorf("Prepare() = %+v, want the root and the filesystem it found", report)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Prepare left %d entries in the root, want the probe cleaned up", len(entries))
	}
}

// TestPrepareRefusesAnUnsafeRoot is the ownership and permission attacks on the root at startup: a
// relative path, /dev/shm, a symbolic link, a file, and a root below a directory any account can
// rename it out of are all refused before anything is staged.
func TestPrepareRefusesAnUnsafeRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes and symbolic links")
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
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	// Chmod rather than Mkdir, so the umask cannot quietly close it again.
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	sticky := filepath.Join(base, "sticky")
	if err := os.Mkdir(sticky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		Root     string
		WantText string
		Want     error
	}{{ // Test 0: A relative path is refused rather than resolved against wherever the process is.
		Root: "relative/runfiles", WantText: "not an absolute path", Want: ErrUnsafeRoot,
	}, { // Test 1: /dev/shm is shared by every process on the host.
		Root: "/dev/shm/switchtender", WantText: "/dev/shm", Want: ErrUnsafeRoot,
	}, { // Test 2: A symbolic link can be swapped for another target.
		Root: link, WantText: "not a directory", Want: ErrUnsafeRoot,
	}, { // Test 3: A file is not a directory.
		Root: file, WantText: "not a directory", Want: ErrUnsafeRoot,
	}, { // Test 4: Under a directory anyone can write without the sticky bit, anyone can rename it.
		Root: filepath.Join(open, "runfiles"), WantText: "sticky", Want: ErrUnsafeRoot,
	}, { // Test 5: Under a sticky shared directory, like /tmp, only its owner can rename it.
		Root: filepath.Join(sticky, "runfiles"), Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, err := prepare(test.Root, checks{classify: localFS, lock: lockFile})
			if !errors.Is(err, test.Want) {
				t.Fatalf("prepare(%s) error = %v, want %v", test.Root, err, test.Want)
			}
			if err != nil && !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("prepare(%s) error = %v, want it to say %q", test.Root, err, test.WantText)
			}
		})
	}
}

// TestPrepareRefusesNetworkAndUnknownFilesystems proves the filesystem allowlist: a network
// filesystem is refused, and so is one SwitchTender does not know, even though the lock probe would
// have passed on it, because nobody has shown its locks hold for every case a sweep depends on.
func TestPrepareRefusesNetworkAndUnknownFilesystems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		FS       filesystem
		WantText string
		Want     error
	}{{ // Test 0: NFS is refused by name.
		FS: filesystem{Name: "nfs", Class: classNetwork}, WantText: "network", Want: ErrFilesystem,
	}, { // Test 1: An unknown type is refused although its locks work.
		FS: filesystem{Name: "fuse", Class: classUnknown}, WantText: "does not know", Want: ErrFilesystem,
	}, { // Test 2: A known local type passes and reports itself.
		FS: filesystem{Name: "tmpfs", Class: classLocal, Memory: true}, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "root")
			c := checks{
				classify: func(string) (filesystem, error) { return test.FS, nil },
				lock:     lockFile,
			}
			report, err := prepare(root, c)
			if !errors.Is(err, test.Want) {
				t.Fatalf("prepare() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				if !strings.Contains(err.Error(), test.WantText) ||
					!strings.Contains(err.Error(), "--runfiles-dir") {
					t.Errorf("prepare() error = %v, want it to say %q and name the flag", err,
						test.WantText)
				}
				return
			}
			want := Report{Root: root, Filesystem: test.FS.Name, MemoryBacked: test.FS.Memory}
			if diff := cmp.Diff(want, report); diff != "" {
				t.Errorf("prepare() report (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPrepareProbeFailures proves the startup probe refuses a filesystem whose locks do not hold:
// one that lets a second handle into a held lock, one that never lets a released lock be taken
// again, and one whose lock call fails outright.
func TestPrepareProbeFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Lock stands in for the filesystem's lock call.
		Lock func(*os.File, bool) (bool, error)
		// WantText is what the refusal must say.
		WantText string
	}{{ // Test 0: Every handle gets the lock, so a sweep would delete a live run.
		Lock:     func(*os.File, bool) (bool, error) { return true, nil },
		WantText: "took a lock the first still held",
	}, { // Test 1: A waiting take succeeds and a second is always refused, even after release.
		Lock:     func(_ *os.File, block bool) (bool, error) { return block, nil },
		WantText: "could not be taken again",
	}, { // Test 2: The lock call itself fails.
		Lock:     func(*os.File, bool) (bool, error) { return false, errors.New("ENOLCK") },
		WantText: "ENOLCK",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "root")
			_, err := prepare(root, checks{classify: localFS, lock: test.Lock})
			if !errors.Is(err, ErrLockProbe) || !strings.Contains(err.Error(), test.WantText) {
				t.Fatalf("prepare() error = %v, want the probe to refuse saying %q", err, test.WantText)
			}
			entries, rerr := os.ReadDir(root)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(entries) != 0 {
				t.Errorf("a failed probe left %d entries in the root", len(entries))
			}
		})
	}
}

// TestProbeUsesTheRunDirectoryLock proves the probe exercises the lock run directories really use:
// on this machine's filesystem a second handle opened the way a sweep opens one is kept out of a
// lock taken the way Create takes one.
func TestProbeUsesTheRunDirectoryLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := probeLock(root, lockFile); err != nil {
		t.Fatalf("probeLock() with the real lock error = %v", err)
	}
	d, err := Create(filepath.Join(root, "runs"), "probe")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer func() { _ = d.Remove() }()
	second, err := openLockFile(filepath.Join(d.Path(), lockName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if got, err := lockFile(second, false); err != nil || got {
		t.Errorf("a second handle on a live run's lock = %v, %v, want it kept out", got, err)
	}
}

// TestFilesystemOfThisMachine checks the classifier against the filesystems every machine of each
// kind has, which keeps the tables honest on the platform the tests run on.
func TestFilesystemOfThisMachine(t *testing.T) {
	t.Parallel()
	fs, err := filesystemOf(t.TempDir())
	if err != nil {
		t.Fatalf("filesystemOf(temp) error = %v", err)
	}
	if fs.Class != classLocal {
		t.Errorf("the temporary directory is on %+v, want a known local filesystem", fs)
	}
	switch runtime.GOOS {
	case "linux":
		if got, err := filesystemOf("/proc"); err != nil || got.Class != classUnknown {
			t.Errorf("filesystemOf(/proc) = %+v, %v, want proc refused as unknown", got, err)
		}
	case "darwin":
		if got, err := filesystemOf("/dev"); err != nil || got.Class != classUnknown {
			t.Errorf("filesystemOf(/dev) = %+v, %v, want devfs refused as unknown", got, err)
		}
	}
}
