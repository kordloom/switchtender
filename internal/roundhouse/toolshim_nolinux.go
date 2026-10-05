//go:build unix && !linux

package roundhouse

import "syscall"

// parentDeathSignal is zero where the kernel has no parent-death signal, so the shim watches only
// the pipe its executor holds open.
const parentDeathSignal syscall.Signal = 0

// setParentDeathSignal does nothing where the kernel has no parent-death signal.
func setParentDeathSignal(*syscall.SysProcAttr) {}

// selfExecutable returns empty, so the shim runs from os.Executable.
func selfExecutable() string { return "" }
