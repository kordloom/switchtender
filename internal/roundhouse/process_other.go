//go:build !unix

package roundhouse

import (
	"os/exec"
	"time"
)

// processWaitDelay bounds how long Wait blocks after a kill before the child's I/O pipes are closed.
const processWaitDelay = 10 * time.Second

// configureProcessGroup bounds the wait after a cancel. Process-group signaling is Unix only, so on
// other platforms the default context kill applies.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = processWaitDelay
}
