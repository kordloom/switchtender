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
}

// newAnthropic builds an Anthropic provider. It requires an API key and defaults the model and URL.
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
		apiKey: apiKey,
		model:  model,
		url:    strings.TrimRight(url, "/"),
		client: newClient(),
	}, nil
}

// Complete sends the prompt to the Messages API and returns the concatenated text reply. A reply
// cut off at the token cap is marked so the reader knows it is incomplete.
func (a *anthropic) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      a.model,
		"max_tokens": anthropicMaxTokens,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
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
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: anthropic: %s", ErrDecode, err)
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	if out.StopReason == "max_tokens" {
		b.WriteString("\n[reply truncated at the token limit]")
	}
	return b.String(), nil
}
