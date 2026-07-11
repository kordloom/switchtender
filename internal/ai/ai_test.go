package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Provider string
		Key      string
		WantNil  bool
		Want     error
	}{
		{Name: "empty is off", Provider: "", WantNil: true},                           // Test 0.
		{Name: "unknown provider", Provider: "gpt5000", Want: ErrUnknownProvider},     // Test 1.
		{Name: "ollama needs no key", Provider: "ollama"},                             // Test 2.
		{Name: "anthropic needs a key", Provider: "anthropic", Want: errAnthropicKey}, // Test 3.
		{Name: "anthropic with a key", Provider: "anthropic", Key: "sk-test"},         // Test 4.
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			p, err := New(test.Provider, "", "", test.Key)
			if test.Want != nil {
				if !errors.Is(err, test.Want) {
					t.Fatalf("test %d: err = %v, want %v", testNum, err, test.Want)
				}
				return
			}
			if err != nil {
				t.Fatalf("test %d: unexpected err = %v", testNum, err)
			}
			if (p == nil) != test.WantNil {
				t.Errorf("test %d: nil provider = %v, want %v", testNum, p == nil, test.WantNil)
			}
		})
	}
}

func TestOllamaComplete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
			t.Errorf("messages = %+v, want system then user", req.Messages)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": "check the lock file"}})
	}))
	defer srv.Close()

	got, err := newOllama(srv.URL, "llama3.1").Complete(context.Background(), "sys", "why did it fail")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != "check the lock file" {
		t.Errorf("Complete() = %q, want the model reply", got)
	}
}

func TestAnthropicComplete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key = %q, want the configured key", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version header")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"text": "restart the service"}},
		})
	}))
	defer srv.Close()

	p, err := newAnthropic("sk-test", "claude-3-5-sonnet-latest", srv.URL)
	if err != nil {
		t.Fatalf("newAnthropic() error = %v", err)
	}
	got, err := p.Complete(context.Background(), "sys", "why did it fail")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != "restart the service" {
		t.Errorf("Complete() = %q, want the model reply", got)
	}
}
