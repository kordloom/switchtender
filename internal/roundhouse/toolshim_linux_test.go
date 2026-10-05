//go:build linux

package roundhouse

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// parentOf returns the parent process id of pid, read from its stat entry.
func parentOf(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatalf("read the tool's stat: %v", err)
	}
	// The command name sits in parentheses and may hold spaces, so the fields are read after it.
	_, rest, ok := strings.Cut(string(raw), ") ")
	fields := strings.Fields(rest)
	if !ok || len(fields) < 2 {
		t.Fatalf("unreadable stat for %d: %q", pid, raw)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("unreadable parent in the stat for %d: %v", pid, err)
	}
	return ppid
}

// TestAStrayParentDeathSignalLeavesTheToolRunning pins the check behind the parent-death signal.
// The kernel sends it when the thread that started the shim exits, and a thread can exit while the
// executor lives on, so the signal alone is not the executor's death. A shim that stopped its tool
// on it would kill a healthy run. The executor's real death must still stop the tool afterward.
func TestAStrayParentDeathSignalLeavesTheToolRunning(t *testing.T) {
	t.Parallel()
	executor, dir := startShimExecutor(t, `echo $$ > pid; sleep 60; echo late > after`, "")
	pid := waitForToolPID(t, dir)
	shim := parentOf(t, pid)
	if err := syscall.Kill(shim, parentDeathSignal); err != nil {
		t.Fatalf("signal the shim: %v", err)
	}
	time.Sleep(time.Second)
	if processGone(pid) {
		t.Fatal("a parent-death signal sent while the executor was alive stopped the tool")
	}
	if err := executor.Process.Kill(); err != nil {
		t.Fatalf("kill the executor: %v", err)
	}
	_ = executor.Wait()
	deadline := time.Now().Add(processKillGrace + 10*time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatal("the tool outlived its executor after a stray parent-death signal")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
