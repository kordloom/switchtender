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

// defaultOllamaURL is the local Ollama endpoint used when none is configured.
const defaultOllamaURL = "http://localhost:11434"

// defaultOllamaModel is the model run when none is configured.
const defaultOllamaModel = "llama3.1"

// ollama calls a local Ollama server, so a model runs on the operator's own hardware and no run
// data leaves the box.
type ollama struct {
	// url is the Ollama base URL without a trailing slash.
	url string
	// model is the model name to run.
	model string
	// client bounds each request.
	client *http.Client
}

// newOllama builds an Ollama provider, defaulting the URL and model when either is empty.
func newOllama(url, model string) *ollama {
	if url == "" {
		url = defaultOllamaURL
	}
	if model == "" {
		model = defaultOllamaModel
	}
	return &ollama{
		url:    strings.TrimRight(url, "/"),
		model:  model,
		client: newClient(),
	}
}

// Complete sends the prompt to Ollama's chat endpoint and returns the reply.
func (o *ollama) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":  o.model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return "", fmt.Errorf("ollama encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("ollama", resp)
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: ollama: %s", ErrDecode, err)
	}
	return out.Message.Content, nil
}
