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

// configureProcessGroup puts the child in its own process group and, on context cancel, sends SIGKILL
// to the whole group. A run engine must not leak the grandchildren a tool spawns, such as ssh workers
// or a terraform provider plugin, when a run is canceled or times out.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The child leads its own group, so the negative pid signals every process in it.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = processWaitDelay
}
