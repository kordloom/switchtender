//go:build windows

package roundhouse

import (
	"os/exec"
	"strconv"
	"time"
)

// processWaitDelay bounds how long Wait blocks after a kill before the child's I/O pipes are closed.
const processWaitDelay = 10 * time.Second

// configureProcessGroup makes a canceled run take down the whole process tree the child roots, not
// just the child. Windows has no process-group signal like the Unix path, so on cancel the child and
// every descendant are terminated with taskkill. A run engine must not leak the grandchildren a tool
// spawns, such as ssh workers or a terraform provider plugin, when a run is canceled or times out.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// taskkill walks the parent-child tree from the child pid, so /T reaches every descendant
		// and /F forces termination when a process ignores the polite request.
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		return kill.Run()
	}
	cmd.WaitDelay = processWaitDelay
}
