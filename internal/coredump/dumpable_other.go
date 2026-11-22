//go:build !linux

package coredump

import "errors"

// HideEnviron reports errors.ErrUnsupported: only Linux lets a process close its /proc entries to
// other processes of its own account. On macOS a process of the same account can read this one's
// environment, so a run with no execution image can read the key the server was started with.
func HideEnviron() error {
	return errors.ErrUnsupported
}
