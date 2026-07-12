package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/run"
)

func TestExplainRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{
		ID: "run_x", Tool: "bash", Command: "deploy.sh", Status: run.StatusFailed, Error: "exit status 1",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// The fake provider proves the prompt carried the run's error, then returns advice.
	var sawError bool
	provider := ai.ProviderFunc(func(_ context.Context, _, user string) (string, error) {
		sawError = strings.Contains(user, "exit status 1")
		return "The script exited non-zero. Re-run deploy.sh with -x to trace it.", nil
	})

	// Test 0: With a provider, a run is explained.
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run_x/explain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explain status = %d, want 200", rec.Code)
	}
	if !sawError {
		t.Error("prompt did not include the run error")
	}
	var body struct {
		Explanation string `json:"explanation"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Explanation, "deploy.sh") {
		t.Errorf("explanation = %q, want the model reply", body.Explanation)
	}

	// Test 1: With no provider, the endpoint is disabled.
	off := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec = httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run_x/explain", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("explain with no provider status = %d, want 404", rec.Code)
	}
}

// TestBuildExplainPrompt covers the prompt size guards: a long command is capped with a truncation
// note, and a log tail cut inside a multibyte rune drops the partial rune so the prompt stays valid
// UTF-8.
func TestBuildExplainPrompt(t *testing.T) {
	t.Parallel()

	// Test 0: A command longer than the cap is truncated and marked.
	longCmd := strings.Repeat("x", explainCommandCap+500)
	prompt := buildExplainPrompt(&run.Run{Tool: "bash", Command: longCmd, Status: run.StatusFailed}, nil)
	if strings.Contains(prompt, longCmd) {
		t.Error("prompt contains the full command, want it capped")
	}
	if !strings.Contains(prompt, "[truncated]") {
		t.Error("prompt missing the truncation note")
	}

	// Test 1: A short command is included whole with no truncation note.
	prompt = buildExplainPrompt(&run.Run{Tool: "bash", Command: "deploy.sh", Status: run.StatusFailed}, nil)
	if !strings.Contains(prompt, "deploy.sh") || strings.Contains(prompt, "[truncated]") {
		t.Errorf("short command mishandled: %q", prompt)
	}

	// Test 2: A log tail cut mid-rune stays valid UTF-8.
	log := append(bytes.Repeat([]byte("é"), explainLogTail), []byte("tail end")...)
	prompt = buildExplainPrompt(&run.Run{Tool: "bash", Status: run.StatusFailed}, log)
	if !utf8.ValidString(prompt) {
		t.Error("prompt is not valid UTF-8 after tail truncation")
	}
	if !strings.Contains(prompt, "tail end") {
		t.Error("prompt missing the log tail content")
	}
}

// TestHeadBytes covers the rune-safe head cut: under the limit, over the limit, and a limit landing
// inside a multibyte rune.
func TestHeadBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In    string
		Limit int
		Want  string
	}{{ // Test 0: Under the limit passes through.
		In: "short", Limit: 10, Want: "short",
	}, { // Test 1: Over the limit is cut with a note.
		In: "abcdef", Limit: 3, Want: "abc\n[truncated]",
	}, { // Test 2: A cut inside a multibyte rune backs off to the rune boundary.
		In: "aé", Limit: 2, Want: "a\n[truncated]",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := headBytes(test.In, test.Limit)
			if got != test.Want {
				t.Errorf("headBytes(%q, %d) = %q, want %q", test.In, test.Limit, got, test.Want)
			}
		})
	}
}
