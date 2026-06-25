package roundhouse

import "errors"

var (
	// ErrNoPlaybook is returned when a Spec has no playbook path.
	ErrNoPlaybook = errors.New("no playbook")
	// ErrLaunch is returned when the executor could not start or supervise the process.
	ErrLaunch = errors.New("launch error")
)
