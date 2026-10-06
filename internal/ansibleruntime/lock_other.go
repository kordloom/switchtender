//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package ansibleruntime

// lockRoot returns a no-op release on a platform without flock. Install refuses to run on Windows,
// where ansible-core does not run as a control node, and the other platforms here are not ones a
// server is built for.
func lockRoot(string) (func(), error) {
	return func() {}, nil
}
