package event

import "errors"

// ErrParse is returned when an event stream contains a malformed line.
var ErrParse = errors.New("parse event")
