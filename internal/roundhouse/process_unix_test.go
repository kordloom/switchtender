//go:build unix

package roundhouse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestConfigureProcessGroupKillsDescendants verifies a canceled run takes down the whole process
// group, not just the direct child, so the grandchildren a tool spawns do not leak. The script
// backgrounds a long sleep, records its pid, and waits on it; cancellation must kill the sleep too.
func TestConfigureProcessGroupKillsDescendants(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := "sleep 60 & echo $! > " + pidFile + "; wait"
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()

	grandchild := waitForPID(t, pidFile)
	if !processAlive(grandchild) {
		t.Fatalf("grandchild %d not running before cancel", grandchild)
	}

	cancel()
	<-waited

	// The backgrounded grandchild must die with the group. Poll briefly, since the kill is async.
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(grandchild) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(grandchild, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived cancellation, want it killed with the group", grandchild)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForPID polls until the pid file holds a parseable pid or the test times out.
func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the grandchild pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether pid names a live process the test can signal.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
