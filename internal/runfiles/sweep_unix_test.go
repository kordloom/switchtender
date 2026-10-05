//go:build unix

package runfiles

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// startHelper re-executes the test binary as a helper process in mode under root and returns it
// with the line it printed once its work was done. The helper is killed when the test ends.
func startHelper(t *testing.T, root, mode string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperRootEnv+"="+root, helperModeEnv+"="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.HasPrefix(line, "ERR") {
		t.Fatalf("helper in mode %s did not report: %q, %v", mode, line, err)
	}
	return cmd, strings.TrimSpace(line)
}

// kill ends a helper the way an out-of-memory kill or a power cut ends a worker: no cleanup runs.
func kill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

// requireRemovedAfterTwoLooks runs the deletion rule over a directory whose owner is dead: the
// first pass only records it, and the pass a full gap later removes it.
func requireRemovedAfterTwoLooks(t *testing.T, s *Sweeper, clock *fakeClock, dir string) {
	t.Helper()
	if got := mustPass(t, s); got.Removed != 0 || !exists(dir) {
		t.Fatalf("first pass after the owner died = %+v, want it only recorded", got)
	}
	clock.advance(ObservationGap)
	if got := mustPass(t, s); got.Removed != 1 {
		t.Fatalf("pass a gap after the owner died = %+v, want the directory removed", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the dead run's directory survived the sweep: %v", err)
	}
}

// TestSweepAfterCrash kills a process that owns a run directory and proves the sweep removes what
// it wrote, while every pass before the kill, across a quarter of an hour of clock, left it alone.
func TestSweepAfterCrash(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	cmd, path := startHelper(t, root, "own")
	clock := newFakeClock()
	clock.advance(MinAge)
	s := testSweeper(root, clock)
	for range 3 {
		if got := mustPass(t, s); got != (PassResult{Live: 1}) {
			t.Fatalf("pass while the owner lives = %+v, want it live", got)
		}
		clock.advance(ObservationGap)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the live owner's file is gone before the crash: %v", err)
	}
	kill(t, cmd)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the crashed owner's file is already gone, so this proves nothing: %v", err)
	}
	requireRemovedAfterTwoLooks(t, s, clock, filepath.Dir(path))
}

// TestSweepAfterKillDuringStaging kills the owner part way through writing a key, the moment a
// crash leaves the most half-finished state, and proves the partial secret goes with the directory.
func TestSweepAfterKillDuringStaging(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	cmd, path := startHelper(t, root, "stage")
	kill(t, cmd)
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "PRIVATE KEY") {
		t.Fatalf("the half written key = %q, %v, want it on disk before the sweep", body, err)
	}
	clock := newFakeClock()
	clock.advance(MinAge)
	requireRemovedAfterTwoLooks(t, testSweeper(root, clock), clock, filepath.Dir(path))
}

// TestSweepAfterKillDuringConsumption kills the owner while a tool it started is reading the
// credential, and proves the directory is reclaimed although the orphaned tool is still running:
// the owner's lock is the liveness signal, and a tool outliving a dead worker belongs to a run the
// control plane has already taken back.
func TestSweepAfterKillDuringConsumption(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	cmd, line := startHelper(t, root, "consume")
	path, pidText, _ := strings.Cut(line, " ")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("helper reported %q, want a path and a process id", line)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	kill(t, cmd)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the reading tool died with its owner, so this proves nothing: %v", err)
	}
	clock := newFakeClock()
	clock.advance(MinAge)
	requireRemovedAfterTwoLooks(t, testSweeper(root, clock), clock, filepath.Dir(path))
}

// TestSuspendedOwnerKeepsItsDirectory stops the owner as a debugger, a suspended laptop, or a
// paused VM does, and proves an hour of sweeps does not touch its directory, since a stopped
// process still holds its lock. It is reclaimed only once the process is gone.
func TestSuspendedOwnerKeepsItsDirectory(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	cmd, path := startHelper(t, root, "own")
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	s := testSweeper(root, clock)
	for range 12 {
		if got := mustPass(t, s); got != (PassResult{Live: 1}) {
			t.Fatalf("pass over a stopped owner = %+v, want it live", got)
		}
		clock.advance(ObservationGap)
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if got := mustPass(t, s); got != (PassResult{Live: 1}) {
		t.Fatalf("pass after the owner resumed = %+v, want it live", got)
	}
	kill(t, cmd)
	requireRemovedAfterTwoLooks(t, s, clock, filepath.Dir(path))
}

// TestManyWorkersOnOneRoot runs several owner processes against one root, kills half of them, and
// proves the sweep reclaims exactly the directories of the dead ones.
func TestManyWorkersOnOneRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	var live, dead []string
	for i := range 4 {
		cmd, line := startHelper(t, root, "many")
		dirs := strings.Fields(line)
		if i%2 == 0 {
			kill(t, cmd)
			dead = append(dead, dirs...)
			continue
		}
		live = append(live, dirs...)
	}
	clock := newFakeClock()
	clock.advance(MinAge)
	s := testSweeper(root, clock)
	removed := 0
	for range 3 {
		res := mustPass(t, s)
		removed += res.Removed
		if res.Live != len(live) {
			t.Errorf("pass counted %d live directories, want %d", res.Live, len(live))
		}
		clock.advance(ObservationGap)
	}
	got := map[string]bool{}
	for _, d := range append(append([]string(nil), live...), dead...) {
		got[d] = exists(d)
	}
	want := map[string]bool{}
	for _, d := range live {
		want[d] = true
	}
	for _, d := range dead {
		want[d] = false
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("what survived (-want +got):\n%s", diff)
	}
	if removed != len(dead) {
		t.Errorf("removed %d directories, want the %d of the dead workers", removed, len(dead))
	}
}

// TestSweepSkipsWhileAnotherProcessSweeps proves the sweep lock works between processes, which is
// what lets one process per host sweep a given pass: while another process holds it, a pass skips,
// and once that process is gone the lock is free again.
func TestSweepSkipsWhileAnotherProcessSweeps(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	cmd, _ := startHelper(t, root, "sweeplock")
	s := NewSweeper(root)
	if got := mustPass(t, s); !got.Skipped {
		t.Fatalf("pass while another process sweeps = %+v, want it skipped", got)
	}
	kill(t, cmd)
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := mustPass(t, s)
		if !got.Skipped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep lock stayed held after its holder died")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
