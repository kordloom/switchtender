package runfiles

import "errors"

var (
	// ErrUnsafeRoot is returned when the directory run directories live under could be read or
	// replaced by another account: a symbolic link, not a directory, owned by someone else, below a
	// directory another account can rename it out of, or in /dev/shm.
	ErrUnsafeRoot = errors.New("run directory root is not private")
	// ErrCreate is returned when a run directory could not be created and kept.
	ErrCreate = errors.New("run directory could not be created")
	// ErrFilesystem is returned when the root sits on a network filesystem, or on one SwitchTender
	// does not know to be local, where a lock cannot be trusted to say whether a run is alive.
	ErrFilesystem = errors.New("run directory root is not on a known local filesystem")
	// ErrLockProbe is returned when the startup probe shows the root's filesystem does not keep a
	// second handle out of a held lock, which is the only thing that tells a live run from a dead one.
	ErrLockProbe = errors.New("run directory lock probe")
)
