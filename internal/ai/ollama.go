package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultOllamaURL is the local Ollama endpoint used when none is configured.
const defaultOllamaURL = "http://localhost:11434"

// defaultOllamaModel is the model run when none is configured.
const defaultOllamaModel = "llama3.1"

// aiTimeout bounds one completion request, since a local model can be slow.
const aiTimeout = 120 * time.Second

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
		client: &http.Client{Timeout: aiTimeout},
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
		return "", fmt.Errorf("ollama status %d", resp.StatusCode)
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("ollama decode: %w", err)
	}
	return out.Message.Content, nil
}
