package policy

import "errors"

// ErrNotFound is returned when a policy id does not exist.
var ErrNotFound = errors.New("policy not found")
