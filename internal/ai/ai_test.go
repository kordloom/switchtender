package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Provider string
		Model    string
		Key      string
		WantNil  bool
		Want     error
	}{
		{Name: "empty is off", Provider: "", WantNil: true},                                 // Test 0.
		{Name: "unknown provider", Provider: "gpt5000", Want: ErrUnknownProvider},           // Test 1.
		{Name: "ollama needs no key", Provider: "ollama"},                                   // Test 2.
		{Name: "anthropic needs a key", Provider: "anthropic", Want: ErrKey},                // Test 3.
		{Name: "anthropic with a key", Provider: "anthropic", Key: "sk-test"},               // Test 4.
		{Name: "openai needs a key", Provider: "openai", Model: "m", Want: ErrKey},          // Test 5.
		{Name: "openai needs a model", Provider: "openai", Key: "sk-test", Want: ErrModel},  // Test 6.
		{Name: "openai with key and model", Provider: "openai", Key: "sk-test", Model: "m"}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			p, err := New(test.Provider, test.Model, "", test.Key)
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

// TestRegister adds a provider through the registry and confirms New builds it, then that an empty
// or duplicate name panics. It does not call t.Parallel: it writes the package registry, so it runs
// in the sequential phase before the parallel tests that read the registry resume.
func TestRegister(t *testing.T) {
	fake := ProviderFunc(func(context.Context, string, string) (string, error) { return "ok", nil })
	Register("faketest", func(_, _, _ string) (Provider, error) { return fake, nil })

	p, err := New("faketest", "", "", "")
	if err != nil {
		t.Fatalf("New(faketest) error = %v", err)
	}
	if p == nil {
		t.Fatal("New(faketest) = nil, want the registered provider")
	}

	tests := []struct {
		Name string
		Reg  string
	}{ // Test 0: Empty name is a programming error.
		{Name: "empty name", Reg: ""},
		// Test 1: A name that is already taken is a programming error.
		{Name: "duplicate name", Reg: "ollama"},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("test %d: Register(%q) did not panic", testNum, test.Reg)
				}
			}()
			Register(test.Reg, func(_, _, _ string) (Provider, error) { return nil, nil })
		})
	}
}

// TestNames lists the registered providers, sorted, including the built-ins.
func TestNames(t *testing.T) {
	got := Names()
	for _, want := range []string{"anthropic", "ollama", "openai"} {
		if !slices.Contains(got, want) {
			t.Errorf("Names() = %v, missing %q", got, want)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("Names() = %v, want sorted", got)
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
			Model     string                           `json:"model"`
			MaxTokens int                              `json:"max_tokens"`
			System    string                           `json:"system"`
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

// TestAnthropicRefusal confirms a safety decline, a 200 with a refusal stop reason, becomes
// ErrRefused carrying the category, not a silent empty reply.
func TestAnthropicRefusal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":      []map[string]string{},
			"stop_reason":  "refusal",
			"stop_details": map[string]string{"category": "cyber"},
		})
	}))
	defer srv.Close()

	p, err := newAnthropic("sk-test", "", srv.URL)
	if err != nil {
		t.Fatalf("newAnthropic() error = %v", err)
	}
	_, err = p.Complete(context.Background(), "sys", "restart nginx on the web hosts")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Complete() error = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "cyber") {
		t.Errorf("error = %v, want it to carry the refusal category", err)
	}
}

// TestAnthropicFallbackOptIn covers the server-side fallback opt-in: a Fable model sends the beta
// header and a fallback in the body, and the default Opus model does neither.
func TestAnthropicFallbackOptIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Model        string
		WantFallback bool
	}{ // Test 0 through 2.
		{Name: "fable opts in", Model: "claude-fable-5", WantFallback: true},
		{Name: "mythos opts in", Model: "claude-mythos-5", WantFallback: true},
		{Name: "opus does not", Model: "claude-opus-4-8", WantFallback: false},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			var gotBeta string
			var body struct {
				Fallbacks []struct {
					Model string `json:"model"`
				} `json:"fallbacks"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBeta = r.Header.Get("anthropic-beta")
				_ = json.NewDecoder(r.Body).Decode(&body)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"content":     []map[string]string{{"type": "text", "text": "ok"}},
					"stop_reason": "end_turn",
				})
			}))
			defer srv.Close()

			p, err := newAnthropic("sk-test", test.Model, srv.URL)
			if err != nil {
				t.Fatalf("test %d: newAnthropic() error = %v", testNum, err)
			}
			if _, err := p.Complete(context.Background(), "sys", "user"); err != nil {
				t.Fatalf("test %d: Complete() error = %v", testNum, err)
			}
			hasFallback := len(body.Fallbacks) == 1 && body.Fallbacks[0].Model == "claude-opus-4-8"
			if hasFallback != test.WantFallback {
				t.Errorf("test %d: fallback in body = %v, want %v", testNum, hasFallback, test.WantFallback)
			}
			hasBeta := strings.Contains(gotBeta, "server-side-fallback")
			if hasBeta != test.WantFallback {
				t.Errorf("test %d: beta header = %q, want fallback %v", testNum, gotBeta, test.WantFallback)
			}
		})
	}
}

// TestAnthropicFallbackRescue proves a fallback reply, a fallback marker block followed by text,
// returns the fallback model's text rather than an error.
func TestAnthropicFallbackRescue(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "fallback", "from": map[string]string{"model": "claude-fable-5"}},
				{"type": "text", "text": "restart nginx with systemctl"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	p, err := newAnthropic("sk-test", "claude-fable-5", srv.URL)
	if err != nil {
		t.Fatalf("newAnthropic() error = %v", err)
	}
	got, err := p.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != "restart nginx with systemctl" {
		t.Errorf("Complete() = %q, want the fallback model's text with the marker skipped", got)
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

// TestAnthropicEmptyReply confirms a non-refusal reply with no text block is surfaced as a decode
// error rather than returned as a blank, successful-looking answer.
func TestAnthropicEmptyReply(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]string{},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	p, err := newAnthropic("sk-test", "", srv.URL)
	if err != nil {
		t.Fatalf("newAnthropic() error = %v", err)
	}
	if _, err := p.Complete(context.Background(), "sys", "why"); !errors.Is(err, ErrDecode) {
		t.Fatalf("Complete() error = %v, want ErrDecode", err)
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

// TestOpenAIComplete asserts the Bearer header, the request body shape, and the reply extraction.
func TestOpenAIComplete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want the bearer key", r.Header.Get("Authorization"))
		}
		var req struct {
			Model    string                           `json:"model"`
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-model" || len(req.Messages) != 2 || req.Messages[0].Role != "system" {
			t.Errorf("request body = %+v, want the model and system then user messages", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "raise the ulimit"}}},
		})
	}))
	defer srv.Close()

	p, err := newOpenAI("sk-test", "test-model", srv.URL)
	if err != nil {
		t.Fatalf("newOpenAI() error = %v", err)
	}
	got, err := p.Complete(context.Background(), "sys", "why did it fail")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != "raise the ulimit" {
		t.Errorf("Complete() = %q, want the model reply", got)
	}
}

// TestOpenAITruncationNote confirms a reply cut off at the token cap is marked as incomplete.
func TestOpenAITruncationNote(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "partial advice"},
				"finish_reason": "length",
			}},
		})
	}))
	defer srv.Close()

	p, err := newOpenAI("sk-test", "test-model", srv.URL)
	if err != nil {
		t.Fatalf("newOpenAI() error = %v", err)
	}
	got, err := p.Complete(context.Background(), "sys", "why")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if !strings.Contains(got, "partial advice") || !strings.Contains(got, "[reply truncated") {
		t.Errorf("Complete() = %q, want the reply with a truncation note", got)
	}
}

// TestOpenAIErrorPaths covers a non-200 reply, malformed JSON, an empty choices list, and a
// redirect that must not be followed so the bearer key cannot be replayed to another host.
func TestOpenAIErrorPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Handler http.HandlerFunc
		Want    error
	}{{ // Test 0: A non-200 reply is a status error.
		Name: "status with body",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
		},
		Want: ErrStatus,
	}, { // Test 1: A malformed reply is a decode error.
		Name: "malformed json",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices": [`))
		},
		Want: ErrDecode,
	}, { // Test 2: An empty choices list is a decode error, not a panic.
		Name: "no choices",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices": []}`))
		},
		Want: ErrDecode,
	}, { // Test 3: A redirect is returned as a status error, never followed.
		Name: "redirect not followed",
		Handler: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.invalid/steal", http.StatusFound)
		},
		Want: ErrStatus,
	}, { // Test 4: A choice with empty content and no truncation is a decode error, not a blank reply.
		Name: "empty content",
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices": [{"message": {"content": ""}, "finish_reason": "stop"}]}`))
		},
		Want: ErrDecode,
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(test.Handler)
			defer srv.Close()
			p, err := newOpenAI("sk-test", "m", srv.URL)
			if err != nil {
				t.Fatalf("test %d: newOpenAI() error = %v", testNum, err)
			}
			if _, err = p.Complete(context.Background(), "sys", "user"); !errors.Is(err, test.Want) {
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
