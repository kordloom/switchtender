package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// defaultAnthropicURL is the Anthropic API base URL.
const defaultAnthropicURL = "https://api.anthropic.com"

// defaultAnthropicModel is used when no model is configured.
const defaultAnthropicModel = "claude-opus-4-8"

// anthropicVersion is the required API version header value.
const anthropicVersion = "2023-06-01"

// anthropicMaxTokens caps the completion length, enough for a triage summary.
const anthropicMaxTokens = 1024

// fallbackBeta enables server-side fallbacks, so a request the primary model declines is re-served
// by the fallback model in the same call.
const fallbackBeta = "server-side-fallback-2026-06-01"

// fallbackModel answers when a request to a classifier-heavy model is declined. Yardmaster sends
// infrastructure and automation content, which a safety classifier can misread as a false
// positive, so a decline is retried here before the caller ever sees it.
const fallbackModel = "claude-opus-4-8"

// anthropic calls the Anthropic Messages API. Run data leaves the box, so it is masked before it is
// sent, and a local provider is preferred when privacy matters.
type anthropic struct {
	// apiKey authenticates to the API. It is never serialized or logged.
	apiKey string
	// model is the model name.
	model string
	// url is the API base URL without a trailing slash.
	url string
	// client bounds each request.
	client *http.Client
	// fallback opts into server-side fallbacks, set for models whose safety classifiers can decline
	// benign automation content.
	fallback bool
}

// newAnthropic builds an Anthropic provider. It requires an API key and defaults the model and URL.
// A Fable or Mythos model opts into server-side fallbacks, since those models run the classifiers
// that can decline automation content by mistake.
func newAnthropic(apiKey, model, url string) (*anthropic, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%w: anthropic", ErrKey)
	}
	if model == "" {
		model = defaultAnthropicModel
	}
	if url == "" {
		url = defaultAnthropicURL
	}
	return &anthropic{
		apiKey:   apiKey,
		model:    model,
		url:      strings.TrimRight(url, "/"),
		client:   newClient(),
		fallback: wantsFallback(model),
	}, nil
}

// wantsFallback reports whether a model should opt into server-side fallbacks. The Fable and Mythos
// models run aggressive safety classifiers that can decline benign automation content, and they are
// served only on the first-party API where server-side fallbacks are available.
func wantsFallback(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "claude-fable") || strings.HasPrefix(m, "claude-mythos")
}

// Complete sends the prompt to the Messages API and returns the concatenated text reply. A reply
// cut off at the token cap is marked so the reader knows it is incomplete. A safety decline, which
// automation content can trip as a false positive, returns ErrRefused after any configured fallback
// has been tried.
func (a *anthropic) Complete(ctx context.Context, system, user string) (string, error) {
	payload := map[string]any{
		"model":      a.model,
		"max_tokens": anthropicMaxTokens,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	}
	if a.fallback {
		payload["fallbacks"] = []map[string]string{{"model": fallbackModel}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("anthropic encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("anthropic request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	if a.fallback {
		req.Header.Set("anthropic-beta", fallbackBeta)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("anthropic", resp)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason  string `json:"stop_reason"`
		StopDetails struct {
			Category string `json:"category"`
		} `json:"stop_details"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: anthropic: %s", ErrDecode, err)
	}
	// A safety decline is a successful HTTP response with a refusal stop reason, so it is caught
	// here rather than by the status check. Reading only text blocks skips the fallback marker.
	if out.StopReason == "refusal" {
		if out.StopDetails.Category != "" {
			return "", fmt.Errorf("%w: %s", ErrRefused, out.StopDetails.Category)
		}
		return "", ErrRefused
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	// A non-refusal reply with no text is surfaced rather than returned blank, so a caller never
	// renders a successful-looking empty answer. A token-cap cutoff is marked instead.
	if b.Len() == 0 && out.StopReason != "max_tokens" {
		return "", fmt.Errorf("%w: anthropic: empty reply (stop_reason %q)", ErrDecode, out.StopReason)
	}
	if out.StopReason == "max_tokens" {
		b.WriteString(truncationNote)
	}
	return b.String(), nil
}
