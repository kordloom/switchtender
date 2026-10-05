//go:build unix

package roundhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A tool used to run as a direct child of the executor in a process group of its own, and nothing
// tied the group's life to the executor's. An executor killed outright, by the out-of-memory
// killer, a power cut, or kill -9, left the play carrying on with no executor, no lease, no log,
// and no record, changing hosts after the control node had already recorded the run interrupted. A
// rerun of the interrupted run, which is what the reliability page tells an operator to do, then
// had two plays changing the same hosts at once.
//
// So every tool runs under a shim: this same binary, started again with toolShimArg, which leads
// the tool's process group and starts the tool inside it. The shim holds the read end of a pipe
// whose only write end stays in the executor, so it reads end of file the moment the executor dies,
// however it dies. On Linux the kernel's parent-death signal tells it as well. Either way it stops
// the group the way a cancel does, a terminate signal and then a kill once processKillGrace has
// passed, after running whatever cleanup the runner registered, such as removing a container.

const (
	// toolShimArg is the first argument that makes this binary run as a tool shim instead of
	// itself. It carries a version, so a binary replaced on disk by an upgrade never misreads the
	// arguments an older one passed.
	toolShimArg = "__switchtender-tool-shim-v1__"
	// shimWatchFD is the descriptor the shim watches: the read end of a pipe whose only write end is
	// held by the process that started the shim.
	shimWatchFD = 3
	// shimReportFD is the descriptor the shim reports a failed tool start on. It is closed once the
	// tool is running.
	shimReportFD = 4
	// shimStartFailed is the exit code of a shim that could not start its tool.
	shimStartFailed = 127
	// shimCleanupTimeout bounds each cleanup command the shim runs for an orphaned tool.
	shimCleanupTimeout = 30 * time.Second
)

// init runs the tool shim when this binary was started as one, before anything else in the program
// runs. It lives here, beside the code that starts shims, so every binary that can start a tool can
// also be one.
func init() {
	if len(os.Args) > 1 && os.Args[1] == toolShimArg {
		os.Exit(runToolShim(os.Args[2:]))
	}
}

// shimExecutable is the binary a shim runs as, empty when it cannot be found. On Linux it is
// /proc/self/exe, which keeps naming the running binary after an upgrade replaces the file on disk.
var shimExecutable = sync.OnceValue(func() string {
	if path := selfExecutable(); path != "" {
		return path
	}
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
})

// toolSupervision is the executor's side of one tool started under the shim.
type toolSupervision struct {
	// alive is the write end of the pipe the shim watches. It stays open, in this process alone, for
	// as long as the tool may run, and the kernel closes it when this process dies.
	alive *os.File
	// watch is the read end the shim inherits, closed here once the shim has started.
	watch *os.File
	// started is the read end of the shim's start report.
	started *os.File
	// report is the write end the shim inherits for its start report, closed here once the shim has
	// started.
	report *os.File
}

// superviseUnderShim rewrites cmd to start its tool under the shim and returns the executor's side
// of the supervision, or nil when the tool runs directly: a command that already failed to resolve
// its binary, which Start reports as it always did, one that passes descriptors of its own, or a
// binary that cannot find its own executable. cleanup lists commands the shim runs if this process
// dies while the tool is still running.
func superviseUnderShim(cmd *exec.Cmd, cleanup [][]string) (*toolSupervision, error) {
	shim := shimExecutable()
	if cmd.Err != nil || len(cmd.ExtraFiles) > 0 || shim == "" {
		return nil, nil
	}
	plan, err := json.Marshal(cleanup)
	if err != nil {
		return nil, fmt.Errorf("supervise tool: %w", err)
	}
	watch, alive, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("supervise tool: %w", err)
	}
	started, report, err := os.Pipe()
	if err != nil {
		_ = watch.Close()
		_ = alive.Close()
		return nil, fmt.Errorf("supervise tool: %w", err)
	}
	args := append([]string{shim, toolShimArg, strconv.Itoa(os.Getpid()), string(plan), cmd.Path},
		cmd.Args...)
	cmd.Path, cmd.Args = shim, args
	cmd.ExtraFiles = []*os.File{watch, report}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	setParentDeathSignal(cmd.SysProcAttr)
	return &toolSupervision{alive: alive, watch: watch, started: started, report: report}, nil
}

// close releases every descriptor this side still holds. Closing alive is what tells a shim whose
// tool is somehow still running that nobody is supervising it any more.
func (s *toolSupervision) close() {
	for _, f := range []*os.File{s.watch, s.report, s.started, s.alive} {
		_ = f.Close()
	}
}

// startReport closes the descriptors the shim inherited and waits for the shim to say whether its
// tool started, returning why it did not, or nil once it is running. A shim that dies before it
// reports reads as started, and its exit status then says what happened.
func (s *toolSupervision) startReport() error {
	_ = s.watch.Close()
	_ = s.report.Close()
	msg, _ := io.ReadAll(s.started)
	_ = s.started.Close()
	if text := strings.TrimSpace(string(msg)); text != "" {
		return errors.New(text)
	}
	return nil
}

// runSupervised runs cmd to completion under the shim when it can, and directly otherwise. A tool
// the shim could not start is reported as the error, never as the shim's exit status.
func runSupervised(cmd *exec.Cmd, cleanup [][]string) error {
	sup, err := superviseUnderShim(cmd, cleanup)
	if err != nil {
		return err
	}
	if sup == nil {
		return cmd.Run()
	}
	defer sup.close()
	if err := cmd.Start(); err != nil {
		return err
	}
	startErr := sup.startReport()
	waitErr := cmd.Wait()
	if startErr != nil {
		return fmt.Errorf("start tool: %w", startErr)
	}
	return waitErr
}

// toolShim is one shim's state: the process that started it, and what to run if that process dies.
type toolShim struct {
	// parent is the process id of the executor that started the shim.
	parent int
	// cleanup are the commands to run once the tool is stopped after its executor died.
	cleanup [][]string
	// tool is the tool the shim started.
	tool *exec.Cmd
}

// runToolShim is the shim's whole life: start the tool, then either mirror how it ended or stop it
// when the executor dies first. args are the executor's process id, the cleanup commands as JSON,
// the tool's path, and its argument list.
func runToolShim(args []string) int {
	report := os.NewFile(shimReportFD, "tool-start-report")
	refuse := func(why string) int {
		if report != nil {
			_, _ = io.WriteString(report, why)
			_ = report.Close()
		}
		return shimStartFailed
	}
	if len(args) < 4 {
		return refuse("the tool shim was started without its tool")
	}
	parent, err := strconv.Atoi(args[0])
	if err != nil {
		return refuse("the tool shim was started without its executor's process id")
	}
	sh := &toolShim{parent: parent}
	if err := json.Unmarshal([]byte(args[1]), &sh.cleanup); err != nil {
		return refuse("the tool shim could not read its cleanup: " + err.Error())
	}
	// Neither descriptor may reach the tool. A tool holding the watched pipe open would hide the
	// executor's death from the shim, and one holding the report would hold the executor's start
	// waiting until the tool exited.
	syscall.CloseOnExec(shimWatchFD)
	syscall.CloseOnExec(shimReportFD)
	orphaned := sh.watchExecutor(os.NewFile(shimWatchFD, "executor-watch"))
	// The terminate and interrupt signals a cancel sends reach the whole group, so the tool hears
	// them directly. The shim catches them rather than dying, and goes on waiting to report how the
	// tool ended. Catching rather than ignoring them leaves them at their defaults in the tool.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	select {
	case <-orphaned:
		return refuse("the executor that started this tool exited before it began")
	default:
	}
	if os.Getppid() != sh.parent {
		return refuse("the executor that started this tool exited before it began")
	}
	sh.tool = &exec.Cmd{Path: args[2], Args: args[3:], Stdin: os.Stdin, Stdout: os.Stdout,
		Stderr: os.Stderr}
	if err := sh.tool.Start(); err != nil {
		return refuse(err.Error())
	}
	_ = report.Close()

	done := make(chan struct{})
	go func() {
		_ = sh.tool.Wait()
		close(done)
	}()
	select {
	case <-done:
		return exitLike(sh.tool.ProcessState)
	case <-orphaned:
	}
	sh.stop(done)
	return 1
}

// watchExecutor returns a channel that is closed once the executor that started the shim is gone:
// the watched pipe reads end of file, which the kernel brings about by closing the executor's write
// end when it exits, or, where the platform has one, the parent-death signal arrives and the shim's
// parent is no longer that executor.
func (sh *toolShim) watchExecutor(watch *os.File) <-chan struct{} {
	orphaned := make(chan struct{})
	var once sync.Once
	gone := func() { once.Do(func() { close(orphaned) }) }
	go func() {
		_, _ = io.Copy(io.Discard, watch)
		gone()
	}()
	if parentDeathSignal != 0 {
		deaths := make(chan os.Signal, 1)
		signal.Notify(deaths, parentDeathSignal)
		go func() {
			// The kernel sends the signal when the thread that started the shim exits, which can
			// happen while the executor lives on, so it counts only once the shim's parent changed.
			for range deaths {
				if os.Getppid() != sh.parent {
					gone()
					return
				}
			}
		}()
	}
	return orphaned
}

// stop ends an orphaned tool the way a cancel does: a terminate signal to its whole group, a kill
// once processKillGrace has passed or the tool has exited, whichever is first, and the registered
// cleanup in between. done is closed when the tool exits.
func (sh *toolShim) stop(done <-chan struct{}) {
	sh.signalGroup(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(processKillGrace):
	}
	for _, c := range sh.cleanup {
		if len(c) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), shimCleanupTimeout)
		_ = exec.CommandContext(ctx, c[0], c[1:]...).Run()
		cancel()
	}
	// The shim is in the group it leads, so this ends the shim as well.
	sh.signalGroup(syscall.SIGKILL)
}

// signalGroup sends sig to every process in the group the shim leads. A shim that does not lead a
// group of its own signals only its tool, so it can never reach the executor's group.
func (sh *toolShim) signalGroup(sig syscall.Signal) {
	if pgid := syscall.Getpgrp(); pgid == os.Getpid() {
		_ = syscall.Kill(-pgid, sig)
		return
	}
	_ = sh.tool.Process.Signal(sig)
}

// exitLike ends the shim the way its tool ended, so the executor reads the same outcome it read
// when it supervised the tool directly: the tool's exit code, or, for a tool ended by a signal, an
// end by a signal.
func exitLike(state *os.ProcessState) int {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return state.ExitCode()
}
