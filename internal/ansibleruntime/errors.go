package ansibleruntime

import "errors"

var (
	// ErrLock is returned for a requirements lock that pip could install from without checking a
	// hash, or from somewhere other than the package index.
	ErrLock = errors.New("invalid ansible runtime lock")
	// ErrVersion is returned when the requested ansible-core version is not a supported release.
	ErrVersion = errors.New("unsupported ansible-core version")
	// ErrPython is returned when no Python the release runs on is available to build it with.
	ErrPython = errors.New("no supported python3")
	// ErrInstall is returned when building the environment or installing into it fails.
	ErrInstall = errors.New("ansible runtime install failed")
	// ErrHashMismatch is returned when a downloaded file does not match the hash its lock records.
	ErrHashMismatch = errors.New("a downloaded file did not match the hash in the lock")
	// ErrVerify is returned when an installed runtime does not report the release it was built for.
	ErrVerify = errors.New("ansible runtime verification failed")
	// ErrNotInstalled is returned when a named managed runtime is not installed.
	ErrNotInstalled = errors.New("ansible runtime not installed")
	// ErrNotManaged is returned for a directory in the runtime's place that it did not create, which
	// is left alone rather than replaced or removed.
	ErrNotManaged = errors.New("not a managed ansible runtime")
	// ErrCommandMissing is returned when an Ansible command is not where it is looked for.
	ErrCommandMissing = errors.New("ansible command not found")
	// ErrUnsupported is returned on a platform ansible-core does not run on as a control node.
	ErrUnsupported = errors.New("ansible-core does not run on this platform")
	// ErrUntrusted is returned for a runtime path another account owns or can write, which is never
	// executed from.
	ErrUntrusted = errors.New("ansible runtime is not trusted")
	// ErrUnusable is returned for an Ansible command started from a runtime selected but unusable,
	// such as a managed runtime in use that is broken, so the run fails rather than running another
	// Ansible.
	ErrUnusable = errors.New("the selected ansible cannot be used")
	// ErrInUse is returned when removing a runtime a run is using.
	ErrInUse = errors.New("ansible runtime in use")
)
