//go:build !unix

// Package coredump keeps a crashing SwitchTender process, and every tool it starts, from writing
// its memory to disk. A server or worker holds decrypted credentials in memory while a run
// executes, and so do the tools it hands them to, so a core file would put them on disk where no
// cleanup reaches.
package coredump

import "errors"

// Disable reports errors.ErrUnsupported: this platform has no core file limit to set. On Windows,
// Windows Error Reporting can keep a dump of a crashing process, so its settings are what to check
// on a host that runs SwitchTender.
func Disable() error {
	return errors.ErrUnsupported
}
