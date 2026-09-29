package cmd

import "errors"

// ErrUsage is returned when CLI arguments or flags are invalid.
var ErrUsage = errors.New("invalid usage")
