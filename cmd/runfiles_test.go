package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/runfiles"
)

// TestServeAndWorkerRefuseAnUnsafeRunFilesRoot proves both executors prove their run files root
// before serving: a root that is a symbolic link, which another account could repoint, stops serve
// and worker alike with the reason, rather than staging keys there.
func TestServeAndWorkerRefuseAnUnsafeRunFilesRoot(t *testing.T) {
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
	tests := []struct {
		// Name labels the executor.
		Name string
		// Run starts it.
		Run func() error
	}{{ // Test 0: The server refuses.
		Name: "serve", Run: func() error { return runServe(testCommand(), nil) },
	}, { // Test 1: A worker refuses before it asks for a license.
		Name: "worker", Run: func() error { return runWorker(testCommand(), nil) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			// Not parallel: the commands read package-level flag variables.
			setString(t, &containerRuntime, "docker")
			setString(t, &containerPullPolicy, "missing")
			setString(t, &containerRunFilesSize, "64m")
			setString(t, &notifyOn, "failure")
			setString(t, &serveDB, tempDB(t))
			setString(t, &workerDB, tempDB(t))
			setString(t, &runFilesDir, link)
			err := test.Run()
			if !errors.Is(err, runfiles.ErrUnsafeRoot) || !strings.Contains(err.Error(), link) {
				t.Fatalf("%s: error = %v, want the linked root refused by name", test.Name, err)
			}
		})
	}
}

// TestContainerRunFilesSizeAcceptsTmpfsSizes pins the sizes the flag takes: what the runtimes pass
// to the kernel's tmpfs, and nothing that would turn into an unsized mount or a runtime error.
func TestContainerRunFilesSizeAcceptsTmpfsSizes(t *testing.T) {
	for _, ok := range []string{"64m", "1g", "65536k", "8388608", "2G"} {
		setString(t, &containerRunFilesSize, ok)
		if err := checkContainerRunFilesSize(); err != nil {
			t.Errorf("checkContainerRunFilesSize(%q) error = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{"", "0", "-1m", "64mb", "50%", "1.5g", "64 m"} {
		setString(t, &containerRunFilesSize, bad)
		if err := checkContainerRunFilesSize(); !errors.Is(err, ErrUsage) {
			t.Errorf("checkContainerRunFilesSize(%q) error = %v, want ErrUsage", bad, err)
		}
	}
}

// TestServeAndWorkerTurnOffCoreDumpsFirst proves both executors turn off core dumps as they start,
// before anything else can fail, in a child process of their own since the change is for good. The
// coredump package proves what Disable does to a process and the tools it starts. This proves serve
// and worker call it: each reports it, then refuses a relative run files directory.
func TestServeAndWorkerTurnOffCoreDumpsFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no core limit to turn off")
	}
	for testNum, command := range []string{"serve", "worker"} {
		t.Run(fmt.Sprintf("test %d %s", testNum, command), func(t *testing.T) {
			t.Parallel()
			_, stderr, code := runCLI(t, command, "--runfiles-dir", "relative/runfiles")
			if code == 0 {
				t.Fatalf("%s started with a relative run files directory", command)
			}
			if !strings.Contains(stderr, "core dumps are off for this process and every tool it runs") {
				t.Errorf("%s did not turn off core dumps before refusing:\n%s", command, stderr)
			}
			if !strings.Contains(stderr, "--runfiles-dir") {
				t.Errorf("%s refused without naming --runfiles-dir:\n%s", command, stderr)
			}
		})
	}
}
