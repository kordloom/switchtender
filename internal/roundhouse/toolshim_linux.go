//go:build linux

package roundhouse

import (
	"os"
	"syscall"
)

// parentDeathSignal is the signal the kernel sends a shim when the thread that started it exits.
// The shim treats it as its executor's death only once its parent has changed, since a thread can
// exit while its process lives on.
const parentDeathSignal = syscall.SIGUSR1

// setParentDeathSignal asks the kernel to send the shim parentDeathSignal when its executor dies.
func setParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = parentDeathSignal
}

// selfExecutable returns /proc/self/exe, which names the running binary even after the file it was
// started from is replaced or removed, or empty when the process has no such entry.
func selfExecutable() string {
	const self = "/proc/self/exe"
	if _, err := os.Stat(self); err != nil {
		return ""
	}
	return self
}
