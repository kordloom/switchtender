package policy

import "errors"

// ErrNotFound is returned when a policy id does not exist.
var ErrNotFound = errors.New("policy not found")

// ErrReadOnly is returned when a policy change is attempted against a source that does not accept
// them, which is what a file-backed policy set is: the file is the source of truth, so a change
// belongs in a diff rather than in an API call that would appear to succeed and do nothing.
var ErrReadOnly = errors.New("policies are read-only")

// ErrUnreachable is returned by a policy store that has no way to read the policies, so a caller
// can tell "there are no policies" apart from "I cannot see them".
var ErrUnreachable = errors.New("approval policies are not reachable from this process")
