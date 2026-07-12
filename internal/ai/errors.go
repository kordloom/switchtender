package ai

import "errors"

// Sentinel errors for provider failures, so callers can classify a failure without matching
// message text.
var (
	// ErrUnknownProvider is returned when a configured provider name is not recognized.
	ErrUnknownProvider = errors.New("unknown ai provider")
	// ErrKey is returned when a provider that needs an API key is selected without one.
	ErrKey = errors.New("ai provider needs an api key")
	// ErrModel is returned when a provider that needs an explicit model is selected without one.
	ErrModel = errors.New("ai provider needs a model")
	// ErrStatus is returned when a provider replies with a non-success HTTP status.
	ErrStatus = errors.New("ai provider status")
	// ErrDecode is returned when a provider reply cannot be decoded.
	ErrDecode = errors.New("ai provider decode")
	// ErrRefused is returned when a provider's safety layer declines the request. It is distinct
	// from an outage, so a caller can tell the user the model declined rather than that it is
	// unreachable. Automation content can trip a false positive, so a cloud provider is configured
	// to retry a decline on a fallback model before this surfaces.
	ErrRefused = errors.New("ai provider declined the request")
)
