// Package ai connects Yardmaster to a language model for advisory features such as explaining a
// failed run. It never sits in the execution path: a provider only ever produces text a human
// reads, so runs stay deterministic and the audit trail stays exact.
package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownProvider is returned when a configured provider name is not recognized.
var ErrUnknownProvider = errors.New("unknown ai provider")

// Provider turns a prompt into a completion. One method keeps providers swappable: a local Ollama
// model, a cloud API, or a fake in tests.
type Provider interface {
	// Complete returns the model's response to the system instruction and the user prompt.
	Complete(ctx context.Context, system, user string) (string, error)
}

// ProviderFunc adapts a plain function to a Provider.
type ProviderFunc func(ctx context.Context, system, user string) (string, error)

// Complete calls the underlying function.
func (f ProviderFunc) Complete(ctx context.Context, system, user string) (string, error) {
	return f(ctx, system, user)
}

// New builds a Provider from a provider name and its settings. An empty name returns a nil Provider
// and no error, so AI stays off by default. An unrecognized name returns ErrUnknownProvider.
func New(name, model, url, apiKey string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return nil, nil
	case "ollama":
		return newOllama(url, model), nil
	case "anthropic":
		return newAnthropic(apiKey, model, url)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
}
