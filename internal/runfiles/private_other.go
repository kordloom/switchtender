//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package runfiles

import "errors"

// privateDir refuses every path, since this platform has no owner and mode to prove a runtime
// directory private with, so the root falls back to the account's own temporary directory.
func privateDir(string) error {
	return errors.New("cannot be proved private on this platform")
}

// checkAncestors accepts the path. Windows guards the account's temporary directory with an access
// list rather than an owner and mode, and the other platforms here refuse the root on its
// filesystem.
func checkAncestors(string) error { return nil }
