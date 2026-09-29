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

// defaultOpenAIURL is the OpenAI API base URL. Any OpenAI-compatible server works through --ai-url,
// which covers self-hosted engines and hosted gateways alike.
const defaultOpenAIURL = "https://api.openai.com"

// openai calls an OpenAI-compatible chat completions API. Run data leaves the box when the endpoint
// is hosted, so it is masked before it is sent, and a local provider is preferred when privacy
// matters.
type openai struct {
	// apiKey authenticates to the API. It is never serialized or logged.
	apiKey string
	// model is the model name.
	model string
	// url is the API base URL without a trailing slash.
	url string
	// client bounds each request.
	client *http.Client
}

// newOpenAI builds an OpenAI-compatible provider. It requires an API key and a model, since the
// compatible ecosystem has no universal default, and defaults only the URL.
func newOpenAI(apiKey, model, url string) (*openai, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%w: openai", ErrKey)
	}
	if model == "" {
		return nil, fmt.Errorf("%w: openai", ErrModel)
	}
	if url == "" {
		url = defaultOpenAIURL
	}
	return &openai{
		apiKey: apiKey,
		model:  model,
		url:    strings.TrimRight(url, "/"),
		client: newClient(),
	}, nil
}

// Complete sends the prompt to the chat completions endpoint and returns the reply.
func (o *openai) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": o.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openai request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("openai", resp)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: openai: %s", ErrDecode, err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%w: openai: no choices in reply", ErrDecode)
	}
	// A reply cut off at the token cap is marked so the reader knows it is incomplete, matching the
	// Anthropic provider. A non-truncated empty reply is surfaced rather than returned blank, so a
	// caller never renders a successful-looking empty answer.
	content := out.Choices[0].Message.Content
	if out.Choices[0].FinishReason == "length" {
		return content + truncationNote, nil
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("%w: openai: empty reply (finish_reason %q)", ErrDecode, out.Choices[0].FinishReason)
	}
	return content, nil
}
