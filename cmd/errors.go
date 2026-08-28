package cmd

import "errors"

// ErrUsage is returned when CLI arguments or flags are invalid.
var ErrUsage = errors.New("invalid usage")

// errNoIdentityHome is returned when there is nowhere durable to keep the producer signing
// identity, so the install would otherwise mint one somewhere a restart empties.
var errNoIdentityHome = errors.New("no durable home for the signing identity")
