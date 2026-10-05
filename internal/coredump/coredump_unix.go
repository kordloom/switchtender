//go:build unix

// Package coredump keeps a crashing SwitchTender process, and every tool it starts, from writing
// its memory to disk. A server or worker holds decrypted credentials in memory while a run
// executes, and so do the tools it hands them to, so a core file would put them on disk where no
// cleanup reaches.
package coredump

import "golang.org/x/sys/unix"

// Disable sets the core file size limit of this process to zero, both the soft limit and the hard
// one, so the kernel writes no core file when the process crashes and nothing it runs can raise the
// limit again. Every tool the process starts inherits it, which matters as much: a run's tool holds
// the run's credentials too.
func Disable() error {
	return unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})
}
