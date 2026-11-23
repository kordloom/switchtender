package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestImagePathRefusedWhereTheWorkerAppliesItsImage pins the case the door cannot see. A run sent
// with no image, to a server with no default image, is accepted, because nothing there knows it will
// run in an image. A worker then applies its own default image, and the executor refuses to mount the
// playbook the run named unless an admin in person named it. The refusal is the run's stored error,
// so the person who sent it reads the sentence and the image on the failed run.
func TestImagePathRefusedWhereTheWorkerAppliesItsImage(t *testing.T) {
	t.Parallel()
	const workerImage = "reg.example.com/worker-default:1"
	tests := []struct {
		// PathsByAdmin is the fact the run is sent with.
		PathsByAdmin bool
		// WantRefused reports whether the executor must refuse the mount.
		WantRefused bool
	}{{ // Test 0: Nobody at admin named the playbook, so the worker's image never mounts it.
		PathsByAdmin: false, WantRefused: true,
	}, { // Test 1: An admin in person named it, so the run reaches the container runtime.
		PathsByAdmin: true, WantRefused: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			playbook := filepath.Join(dir, "site.yml")
			writeTestFile(t, playbook, "- hosts: all\n  tasks: []\n")
			bin, calls := stubContainerRuntime(t)
			runner := roundhouse.NewSelectiveRunner(true, bin, "missing", false,
				roundhouse.DefaultContainerLimits())
			store := run.NewMemStore()
			submitter := New(store, runner, nil, WithNoJanitor(),
				WithClaimGate(func() error { return errNoClaim }))
			t.Cleanup(submitter.Close)
			r, err := submitter.Submit(ctx, playbook, "", run.WithTool(run.ToolAnsible),
				run.WithPathsByAdmin(test.PathsByAdmin))
			if err != nil {
				t.Fatalf("Submit() error = %v, want the run accepted with no image known", err)
			}
			executor := New(store, runner, nil, WithDefaultImage(workerImage), WithNoJanitor())
			t.Cleanup(executor.Close)
			final := dispatchtest.WaitTerminal(t, store, r.ID)
			refused := strings.Contains(final.Error, "takes its paths from a project")
			if refused != test.WantRefused {
				t.Fatalf("status %q error %q: refused = %v, want %v", final.Status, final.Error,
					refused, test.WantRefused)
			}
			if !test.WantRefused {
				if called(t, calls) == "" {
					t.Errorf("the container runtime was never called for an admin's run")
				}
				return
			}
			if final.Status != run.StatusFailed {
				t.Errorf("status = %q, want failed", final.Status)
			}
			for _, want := range []string{workerImage, dir} {
				if !strings.Contains(final.Error, want) {
					t.Errorf("stored error %q does not name %q", final.Error, want)
				}
			}
			if got := called(t, calls); got != "" {
				t.Errorf("the container runtime was called despite the refusal:\n%s", got)
			}
		})
	}
}

// stubContainerRuntime writes an executable stand-in for the container CLI that records each call
// to a log and exits 0, and returns its path and the log's path.
func stubContainerRuntime(t *testing.T) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "docker")
	writeTestFile(t, bin, "#!/bin/sh\n[ \"$1\" = stub-warmup ] && exit 0\necho \"$@\" >> "+calls+
		"\nexit 0\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	return bin, calls
}

// called returns what the stand-in runtime logged, or empty when it was never called.
func called(t *testing.T, calls string) string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeTestFile writes content to path or fails the test.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
