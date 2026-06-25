package dispatch

import "errors"

// ErrNoPlaybook is returned when a run is submitted without a playbook path.
var ErrNoPlaybook = errors.New("no playbook")
