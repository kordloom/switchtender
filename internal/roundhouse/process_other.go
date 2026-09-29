//go:build !unix && !windows

package roundhouse

import (
	"os/exec"
	"time"
)

// processWaitDelay bounds how long Wait blocks after a kill before the child's I/O pipes are closed.
const processWaitDelay = 10 * time.Second

// configureProcessGroup bounds the wait after a cancel. Killing the whole process tree is handled
// per platform, on Unix by a process-group signal and on Windows by taskkill; a platform with neither
// falls back to the default context kill of the direct child only.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = processWaitDelay
}
