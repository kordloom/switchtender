//go:build unix

package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shimHelperEnv names the variable that turns this test binary into an executor for the shim tests.
const shimHelperEnv = "ROUNDHOUSE_SHIM_HELPER_DIR"

// TestShimHelperExecutor is not a test of its own. Started by the shim tests as a separate process,
// with shimHelperEnv naming a directory, it runs the tool script in that directory the way every
// runner does and waits on it, standing in for an executor that is about to be killed outright.
func TestShimHelperExecutor(t *testing.T) {
	t.Parallel()
	dir := os.Getenv(shimHelperEnv)
	if dir == "" {
		return
	}
	script, err := os.ReadFile(filepath.Join(dir, "tool.sh"))
	if err != nil {
		os.Exit(3)
	}
	cmd := exec.CommandContext(context.Background(), "sh", "-c", string(script))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), cleanupEnv(dir)...)
	var cleanup [][]string
	if raw, err := os.ReadFile(filepath.Join(dir, "cleanup")); err == nil {
		cleanup = [][]string{strings.Fields(string(raw))}
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	configureProcessGroup(cmd)
	_ = runSupervised(cmd, cleanup)
	os.Exit(0)
}

// cleanupEnv returns the environment entries the helper's tool scripts read.
func cleanupEnv(dir string) []string {
	return []string{"SHIM_TEST_DIR=" + dir}
}

// startShimExecutor starts this test binary as an executor running script under the shim, with
// cleanup as the command the shim runs if the executor dies, and returns the executor and the
// directory the script runs in.
func startShimExecutor(t *testing.T, script, cleanup string) (*exec.Cmd, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte(script), 0o600); err != nil {
		t.Fatalf("write the tool script: %v", err)
	}
	if cleanup != "" {
		if err := os.WriteFile(filepath.Join(dir, "cleanup"), []byte(cleanup), 0o600); err != nil {
			t.Fatalf("write the cleanup: %v", err)
		}
	}
	executor := exec.Command(os.Args[0], "-test.run=^TestShimHelperExecutor$")
	executor.Env = append(os.Environ(), shimHelperEnv+"="+dir)
	if err := executor.Start(); err != nil {
		t.Fatalf("start the executor: %v", err)
	}
	t.Cleanup(func() {
		_ = executor.Process.Kill()
		_ = executor.Wait()
	})
	return executor, dir
}

// waitForToolPID waits for the tool script to record the process id it was told to and returns it.
// The test stops that process's whole group when it ends, so a tool that outlived its executor does
// not outlive the test. The group is never the test's own.
func waitForToolPID(t *testing.T, dir string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(dir, "pid"))
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && perr == nil {
			t.Cleanup(func() {
				if pgid, err := syscall.Getpgid(pid); err == nil && pgid != syscall.Getpgrp() {
					_ = syscall.Kill(-pgid, syscall.SIGKILL)
				}
			})
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the tool never started")
	return 0
}

// processGone reports whether pid names no live process.
func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// TestAToolStopsWhenItsExecutorIsKilled kills an executor outright while its tool runs, the way an
// out-of-memory kill or a power cut ends one, and watches the tool.
//
// The tool used to run in a process group of its own that nothing tied to the executor's life, so
// it carried on with no executor, no lease, and no record, and changed hosts after the control node
// had recorded the run interrupted. The shim that leads the group now stops it the way a cancel
// does: a terminate signal, then a kill for whatever is still there once the grace has passed.
func TestAToolStopsWhenItsExecutorIsKilled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Script string
	}{{ // Test 0: A tool that stops on the terminate signal.
		Name:   "stops on terminate",
		Script: `echo $$ > pid; sleep 60; echo late > after`,
	}, { // Test 1: A tool that ignores the terminate signal is killed once the grace has passed.
		Name:   "ignores terminate",
		Script: `trap '' TERM; echo $$ > pid; sleep 60; echo late > after`,
	}, { // Test 2: A child the tool put in the background goes with it.
		Name:   "background child",
		Script: `(sleep 60; echo late > after) & echo $! > pid; wait`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			executor, dir := startShimExecutor(t, test.Script, "")
			pid := waitForToolPID(t, dir)
			if err := executor.Process.Kill(); err != nil {
				t.Fatalf("kill the executor: %v", err)
			}
			_ = executor.Wait()
			deadline := time.Now().Add(processKillGrace + 10*time.Second)
			for !processGone(pid) {
				if time.Now().After(deadline) {
					t.Fatalf("the tool (pid %d) was still running %s after its executor was killed",
						pid, processKillGrace+10*time.Second)
				}
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(dir, "after")); err == nil {
				t.Error("the tool finished its work after its executor was killed")
			}
		})
	}
}

// TestAnOrphanedToolsCleanupRuns pins the cleanup a runner registers for a tool whose executor
// dies, which is how a container that would otherwise run on under its daemon is removed. The
// command must run once the tool is stopped, with nothing left for the executor to do it.
func TestAnOrphanedToolsCleanupRuns(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "cleaned")
	executor, dir := startShimExecutor(t, `echo $$ > pid; sleep 60`, "touch "+marker)
	pid := waitForToolPID(t, dir)
	if err := executor.Process.Kill(); err != nil {
		t.Fatalf("kill the executor: %v", err)
	}
	_ = executor.Wait()
	deadline := time.Now().Add(processKillGrace + 10*time.Second)
	for {
		if _, err := os.Stat(marker); err == nil && processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the orphaned tool's cleanup never ran (tool gone: %v)", processGone(pid))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// toolCommand builds a tool command the way every runner does, bound to a context.
func toolCommand(name string, args ...string) *exec.Cmd {
	return exec.CommandContext(context.Background(), name, args...)
}

// TestASupervisedToolEndsTheWayItsToolEnds pins that the shim is invisible to the runner: the exit
// code a tool ends with, a tool ended by a signal, a tool that cannot start, and a tool whose
// binary does not exist all read exactly as they did when the runner started the tool itself.
func TestASupervisedToolEndsTheWayItsToolEnds(t *testing.T) {
	t.Parallel()
	notExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatalf("write a file that is not executable: %v", err)
	}
	tests := []struct {
		Name     string
		Cmd      func() *exec.Cmd
		WantExit int
		Want     error
	}{{ // Test 0: A clean exit.
		Name: "success", Cmd: func() *exec.Cmd { return toolCommand("sh", "-c", "exit 0") },
	}, { // Test 1: A failing exit keeps its code.
		Name: "exit code", Cmd: func() *exec.Cmd { return toolCommand("sh", "-c", "exit 7") },
		WantExit: 7,
	}, { // Test 2: A tool ended by a signal reads as one.
		Name:     "signaled",
		Cmd:      func() *exec.Cmd { return toolCommand("sh", "-c", "kill -9 $$") },
		WantExit: -1,
	}, { // Test 3: A binary that exists but cannot be started is a launch failure.
		Name: "cannot start", Cmd: func() *exec.Cmd { return toolCommand(notExecutable) },
		WantExit: -1, Want: ErrLaunch,
	}, { // Test 4: A binary that does not exist is a launch failure.
		Name:     "missing",
		Cmd:      func() *exec.Cmd { return toolCommand("switchtender-no-such-tool") },
		WantExit: -1, Want: ErrLaunch,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			res, err := runProcess(context.Background(), test.Cmd(), io.Discard)
			if !errors.Is(err, test.Want) {
				t.Fatalf("runProcess() error = %v, want %v", err, test.Want)
			}
			if res.ExitCode != test.WantExit {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, test.WantExit)
			}
		})
	}
}

// TestACanceledSupervisedToolStillGetsItsGrace pins that a cancel reaches the tool under the shim
// exactly as it reached it directly: the terminate signal first, so a tool that cleans up on it,
// as terraform releases its state lock, still does, and the runner reads the cancellation.
func TestACanceledSupervisedToolStillGetsItsGrace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c",
		`trap 'echo released > released; exit 0' TERM; echo $$ > pid; while :; do sleep 1; done`)
	cmd.Dir = dir
	done := make(chan error, 1)
	go func() {
		_, err := runProcess(ctx, cmd, io.Discard)
		done <- err
	}()
	waitForToolPID(t, dir)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runProcess() error = %v, want the cancellation", err)
		}
	case <-time.After(processKillGrace + 10*time.Second):
		t.Fatal("the canceled tool never ended")
	}
	if _, err := os.Stat(filepath.Join(dir, "released")); err != nil {
		t.Errorf("the tool did not get the terminate signal it cleans up on: %v", err)
	}
}

// containerHelperEnv names the variable that turns this test binary into an executor running a
// container through the stand-in runtime in the directory it names.
const containerHelperEnv = "ROUNDHOUSE_SHIM_CONTAINER_DIR"

// TestShimHelperContainerExecutor is not a test of its own. Started by the container shim test as a
// separate process, with containerHelperEnv naming the directory that holds a stand-in runtime, it
// runs a container through that runtime the way the container runner does and waits on it.
func TestShimHelperContainerExecutor(t *testing.T) {
	t.Parallel()
	dir := os.Getenv(containerHelperEnv)
	if dir == "" {
		return
	}
	c := newContainerRunner(filepath.Join(dir, "docker"), "missing", false, nil, &pluginCache{},
		DefaultContainerLimits())
	_, _ = c.Run(context.Background(), Spec{Tool: "bash", Command: "echo hi", Image: "alpine:3"},
		io.Discard)
	os.Exit(0)
}

// TestAnOrphanedContainerIsStoppedAndRemoved kills an executor outright while its container runs.
// The container runs under the daemon rather than under the executor, so before the shim it carried
// on with the play after the executor and its client were gone, and nothing ever removed it. The
// shim the client runs under now stops and removes it by name, the way a cancel does.
func TestAnOrphanedContainerIsStoppedAndRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls.log")
	writeStub(t, filepath.Join(dir, "docker"), `#!/bin/sh
[ "$1" = stub-warmup ] && exit 0
echo "$@" >> `+calls+`
if [ "$1" = run ]; then
  echo $$ > `+filepath.Join(dir, "pid")+`
  sleep 60
fi
exit 0
`)
	executor := exec.Command(os.Args[0], "-test.run=^TestShimHelperContainerExecutor$")
	executor.Env = append(os.Environ(), containerHelperEnv+"="+dir)
	if err := executor.Start(); err != nil {
		t.Fatalf("start the executor: %v", err)
	}
	t.Cleanup(func() {
		_ = executor.Process.Kill()
		_ = executor.Wait()
	})
	client := waitForToolPID(t, dir)
	if err := executor.Process.Kill(); err != nil {
		t.Fatalf("kill the executor: %v", err)
	}
	_ = executor.Wait()
	deadline := time.Now().Add(processKillGrace + 20*time.Second)
	for {
		body, _ := os.ReadFile(calls)
		if strings.Contains(string(body), "stop --time") && strings.Contains(string(body), "rm -f") &&
			processGone(client) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the orphaned container was not stopped and removed (client gone: %v):\n%s",
				processGone(client), body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
