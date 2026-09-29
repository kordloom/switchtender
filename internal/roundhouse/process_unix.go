//go:build unix

package roundhouse

import (
	"os/exec"
	"syscall"
	"time"
)

// processWaitDelay bounds how long Wait blocks after a kill for the child to exit before its I/O
// pipes are force-closed, so a process that ignores SIGKILL cannot hang the supervisor forever.
const processWaitDelay = 10 * time.Second

// processKillGrace is how long a canceled tool gets after SIGTERM before the group is SIGKILLed.
// The grace is what lets terraform release its state lock and ansible stop between tasks instead
// of dying mid-write; anything that ignores the term signal is killed when the grace expires.
const processKillGrace = 10 * time.Second

// configureProcessGroup puts the child in its own process group and, on context cancel, signals
// the whole group: SIGTERM first so the tool can stop cleanly, then SIGKILL when the grace runs
// out. A run engine must not leak the grandchildren a tool spawns, such as ssh workers or a
// terraform provider plugin, when a run is canceled or times out.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The child leads its own group, so the negative pid signals every process in it. When
		// the group already exited the escalation kill returns ESRCH and signals nothing.
		pid := cmd.Process.Pid
		err := syscall.Kill(-pid, syscall.SIGTERM)
		time.AfterFunc(processKillGrace, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		return err
	}
	cmd.WaitDelay = processWaitDelay + processKillGrace
}
