package coredump

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// HideEnviron marks this process non-dumpable. The kernel then refuses another process of the same
// account that tries to read this one's /proc entries, environ and mem among them, or to attach to
// it, unless it holds CAP_SYS_PTRACE. A run with no execution image runs as the server's own
// account, so without this one line of shell in a run printed /proc/<server pid>/environ, and with
// it the master key and salt the server was started with.
//
// The flag belongs to this process and is reset by exec, so a tool the process starts runs as it
// always did and reads its own /proc entries as before.
func HideEnviron() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0): %w", err)
	}
	return nil
}
