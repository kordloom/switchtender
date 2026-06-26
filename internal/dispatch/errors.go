package dispatch

import "errors"

var (
	// ErrNoPlaybook is returned when a run is submitted without a playbook path.
	ErrNoPlaybook = errors.New("no playbook")
	// ErrNoHostLister is returned when a split is requested but the runner cannot list hosts.
	ErrNoHostLister = errors.New("host listing unavailable")
)
