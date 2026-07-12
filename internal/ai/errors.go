package ai

import "errors"

// Sentinel errors for provider failures, so callers can classify a failure without matching
// message text.
var (
	// ErrUnknownProvider is returned when a configured provider name is not recognized.
	ErrUnknownProvider = errors.New("unknown ai provider")
	// ErrKey is returned when a provider that needs an API key is selected without one.
	ErrKey = errors.New("ai provider needs an api key")
	// ErrStatus is returned when a provider replies with a non-success HTTP status.
	ErrStatus = errors.New("ai provider status")
	// ErrDecode is returned when a provider reply cannot be decoded.
	ErrDecode = errors.New("ai provider decode")
)
