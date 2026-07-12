// Package ai connects Yardmaster to a language model for advisory features such as explaining a
// failed run. It never sits in the execution path: a provider only ever produces text a human
// reads, so runs stay deterministic and the audit trail stays exact.
package ai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// aiTimeout bounds one completion request, since a local model can be slow.
const aiTimeout = 120 * time.Second

// maxResponseBytes caps how much of a provider reply is decoded, so a broken or hostile endpoint
// cannot exhaust memory on the control plane.
const maxResponseBytes = 1 << 20

// errorBodyCap is how much of a provider error body to keep for the server log.
const errorBodyCap = 2048

// truncationNote marks a reply a provider cut off at its token cap, so the reader knows the answer
// is incomplete instead of treating a mid-sentence stop as the whole reply.
const truncationNote = "\n[reply truncated at the token limit]"

// newClient returns the HTTP client the providers share: a hard timeout, and no redirect
// following, so a misconfigured or hostile endpoint cannot replay credentials elsewhere.
func newClient() *http.Client {
	return &http.Client{
		Timeout: aiTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// statusError builds the error for a non-success provider reply, keeping a short excerpt of the
// body so a bad model name or quota problem is diagnosable from the server log.
func statusError(provider string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyCap))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return fmt.Errorf("%w: %s: %d", ErrStatus, provider, resp.StatusCode)
	}
	return fmt.Errorf("%w: %s: %d: %s", ErrStatus, provider, resp.StatusCode, detail)
}

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
// and no error, so AI stays off by default. An unrecognized name returns ErrUnknownProvider. The
// openai provider speaks the OpenAI-compatible chat completions API, so any compatible server works
// through the URL setting.
func New(name, model, url, apiKey string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return nil, nil
	case "ollama":
		return newOllama(url, model), nil
	case "anthropic":
		return newAnthropic(apiKey, model, url)
	case "openai":
		return newOpenAI(apiKey, model, url)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
}
