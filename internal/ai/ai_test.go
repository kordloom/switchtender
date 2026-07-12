package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		{Name: "anthropic needs a key", Provider: "anthropic", Want: ErrKey}, // Test 3.
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
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    string `json:"system"`
			Messages  []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "" || req.MaxTokens <= 0 || req.System != "sys" {
			t.Errorf("request body = %+v, want model, max_tokens, and system set", req)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("messages = %+v, want one user message", req.Messages)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{
				{"type": "thinking", "text": "hidden"},
				{"type": "text", "text": "restart the service"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	p, err := newAnthropic("sk-test", "", srv.URL)
	if err != nil {
		t.Fatalf("newAnthropic() error = %v", err)
	}
	got, err := p.Complete(context.Background(), "sys", "why did it fail")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != "restart the service" {
		t.Errorf("Complete() = %q, want only the text block", got)
	}
}

// TestAnthropicTruncationNote confirms a reply cut off at the token cap is marked as incomplete.
func TestAnthropicTruncationNote(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]string{{"type": "text", "text": "partial advice"}},
			"stop_reason": "max_tokens",
		})
	}))
	defer srv.Close()

	p, err := newAnthropic("sk-test", "", srv.URL)
	if err != nil {
		t.Fatalf("newAnthropic() error = %v", err)
	}
	got, err := p.Complete(context.Background(), "sys", "why")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if !strings.Contains(got, "partial advice") || !strings.Contains(got, "[reply truncated") {
		t.Errorf("Complete() = %q, want the reply with a truncation note", got)
	}
}

// TestAnthropicErrorPaths covers a non-200 reply keeping the body excerpt, malformed JSON, and a
// redirect that must not be followed so the API key cannot be replayed to another host.
func TestAnthropicErrorPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Handler http.HandlerFunc
		Want    error
		WantMsg string
	}{{ // Test 0: A non-200 reply keeps a body excerpt for the server log.
		Name: "status with body",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
		},
		Want: ErrStatus, WantMsg: "model not found",
	}, { // Test 1: A malformed reply is a decode error.
		Name: "malformed json",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"content": [`))
		},
		Want: ErrDecode,
	}, { // Test 2: A redirect is returned as a status error, never followed.
		Name: "redirect not followed",
		Handler: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.invalid/steal", http.StatusFound)
		},
		Want: ErrStatus, WantMsg: "302",
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(test.Handler)
			defer srv.Close()
			p, err := newAnthropic("sk-test", "", srv.URL)
			if err != nil {
				t.Fatalf("test %d: newAnthropic() error = %v", testNum, err)
			}
			_, err = p.Complete(context.Background(), "sys", "user")
			if !errors.Is(err, test.Want) {
				t.Fatalf("test %d: err = %v, want %v", testNum, err, test.Want)
			}
			if test.WantMsg != "" && !strings.Contains(err.Error(), test.WantMsg) {
				t.Errorf("test %d: err = %v, want it to mention %q", testNum, err, test.WantMsg)
			}
		})
	}
}

// TestOllamaErrorPaths covers a non-200 reply and malformed JSON from the local endpoint.
func TestOllamaErrorPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Handler http.HandlerFunc
		Want    error
	}{{ // Test 0: A non-200 reply is a status error.
		Name: "status",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model missing"}`))
		},
		Want: ErrStatus,
	}, { // Test 1: A malformed reply is a decode error.
		Name: "malformed json",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"message": {`))
		},
		Want: ErrDecode,
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(test.Handler)
			defer srv.Close()
			_, err := newOllama(srv.URL, "m").Complete(context.Background(), "sys", "user")
			if !errors.Is(err, test.Want) {
				t.Fatalf("test %d: err = %v, want %v", testNum, err, test.Want)
			}
		})
	}
}

// TestCompleteContextCanceled proves a canceled request context aborts a hung provider call.
func TestCompleteContextCanceled(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := newOllama(srv.URL, "m").Complete(ctx, "sys", "user"); err == nil {
		t.Fatal("Complete() error = nil, want context cancellation")
	}
}
