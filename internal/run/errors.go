package run

import "errors"

// ErrNotFound is returned when a run does not exist in the store.
var ErrNotFound = errors.New("run not found")
