package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// defaultAnthropicURL is the Anthropic API base URL.
const defaultAnthropicURL = "https://api.anthropic.com"

// defaultAnthropicModel is used when no model is configured.
const defaultAnthropicModel = "claude-3-5-sonnet-latest"

// anthropicVersion is the required API version header value.
const anthropicVersion = "2023-06-01"

// anthropicMaxTokens caps the completion length, enough for a triage summary.
const anthropicMaxTokens = 1024

// errAnthropicKey is returned when the Anthropic provider is selected without an API key.
var errAnthropicKey = errors.New("anthropic needs an api key")

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
		return nil, errAnthropicKey
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
		client: &http.Client{Timeout: aiTimeout},
	}, nil
}

// Complete sends the prompt to the Messages API and returns the concatenated text reply.
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
		return "", fmt.Errorf("anthropic status %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("anthropic decode: %w", err)
	}
	var b strings.Builder
	for _, c := range out.Content {
		b.WriteString(c.Text)
	}
	return b.String(), nil
}
