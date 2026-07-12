package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/run"
)

// TestDraftStep covers the draft endpoint: disabled 404, tool and prompt validation, the happy
// path carrying tool and task to the provider, and a provider failure mapping to a generic 502.
func TestDraftStep(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()

	// Test 0: With no provider, the endpoint is disabled.
	off := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ai/draft",
		strings.NewReader(`{"tool":"bash","prompt":"drain the node"}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("draft with no provider status = %d, want 404", rec.Code)
	}

	var gotUser string
	provider := ai.ProviderFunc(func(_ context.Context, _, user string) (string, error) {
		gotUser = user
		return "```bash\n#!/bin/sh\nkubectl drain node-1\n```", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()

	// Test 1: A tool outside the script set is refused.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ai/draft",
		strings.NewReader(`{"tool":"ansible","prompt":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ansible draft status = %d, want 400", rec.Code)
	}

	// Test 2: An empty description is refused.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ai/draft",
		strings.NewReader(`{"tool":"bash","prompt":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty prompt status = %d, want 400", rec.Code)
	}

	// Test 3: A valid request reaches the provider with the tool and task, and the fenced reply
	// comes back stripped.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ai/draft",
		strings.NewReader(`{"tool":"bash","prompt":"drain the node"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("draft status = %d, want 200", rec.Code)
	}
	if !strings.Contains(gotUser, "Tool: bash") || !strings.Contains(gotUser, "drain the node") {
		t.Errorf("provider prompt = %q, want the tool and the task", gotUser)
	}
	var body struct {
		Draft string `json:"draft"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Draft != "#!/bin/sh\nkubectl drain node-1" {
		t.Errorf("draft = %q, want the fence-stripped script", body.Draft)
	}

	// Test 4: A provider failure maps to a generic 502 without provider internals.
	boom := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "", errors.New("internal provider detail")
	})
	broken := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(boom)).Handler()
	rec = httptest.NewRecorder()
	broken.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ai/draft",
		strings.NewReader(`{"tool":"python","prompt":"parse a csv"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("provider failure status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "internal provider detail") {
		t.Errorf("provider internals leaked: %s", rec.Body.String())
	}
}

// TestStripFences covers unwrapping a markdown code fence: fenced with a language, fenced without
// one, unfenced input, and a fence with no closing line.
func TestStripFences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want string
	}{{ // Test 0: A language-tagged fence unwraps.
		In: "```bash\necho hi\n```", Want: "echo hi",
	}, { // Test 1: A bare fence unwraps.
		In: "```\necho hi\n```", Want: "echo hi",
	}, { // Test 2: Unfenced input passes through.
		In: "echo hi", Want: "echo hi",
	}, { // Test 3: A fence with no closing line keeps the body.
		In: "```python\nprint(1)", Want: "print(1)",
	}, { // Test 4: Surrounding whitespace is trimmed.
		In: "  ```\necho hi\n```  ", Want: "echo hi",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := stripFences(test.In); got != test.Want {
				t.Errorf("stripFences(%q) = %q, want %q", test.In, got, test.Want)
			}
		})
	}
}
