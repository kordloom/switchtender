//go:build !(linux || darwin || freebsd || windows)

package runfiles

// filesystemOf reports every filesystem as unknown on a platform SwitchTender does not ship for,
// so Prepare refuses the root there rather than trusting a lock nobody has shown to hold.
func filesystemOf(string) (filesystem, error) {
	return filesystem{Name: "unknown", Class: classUnknown}, nil
}
