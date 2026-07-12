package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/event"
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

// TestExplainRunGates covers the non-terminal 409, the provider failure 502 with no detail leak,
// and the unknown run 404.
func TestExplainRunGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{ID: "run_going", Tool: "bash", Status: run.StatusRunning}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Save(ctx, &run.Run{ID: "run_done", Tool: "bash", Status: run.StatusFailed}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	boom := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "", errors.New("provider exploded with secret detail")
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(boom)).Handler()

	// Test 0: A run that is still executing is refused with 409.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run_going/explain", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("non-terminal explain status = %d, want 409", rec.Code)
	}

	// Test 1: A provider failure maps to a generic 502 without provider internals.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run_done/explain", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("provider failure status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Errorf("provider internals leaked to the client: %s", rec.Body.String())
	}

	// Test 2: An unknown run is a 404.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run_missing/explain", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown run explain status = %d, want 404", rec.Code)
	}
}

// TestExplainRunCached proves a second explain for the same run reuses the answer instead of
// calling the provider again.
func TestExplainRunCached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{ID: "run_c", Tool: "bash", Status: run.StatusFailed}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	calls := 0
	provider := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		calls++
		return "cached advice", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run_c/explain", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("explain %d status = %d, want 200", i, rec.Code)
		}
	}
	if calls != 1 {
		t.Errorf("provider calls = %d, want 1 (second request served from cache)", calls)
	}
}

// TestBuildExplainPrompt covers the prompt size guards: a long command is capped with a truncation
// note, and a log tail cut inside a multibyte rune drops the partial rune so the prompt stays valid
// UTF-8.
func TestBuildExplainPrompt(t *testing.T) {
	t.Parallel()

	// Test 0: A command longer than the cap is truncated and marked.
	longCmd := strings.Repeat("x", explainCommandCap+500)
	prompt := buildExplainPrompt(&run.Run{Tool: "bash", Command: longCmd, Status: run.StatusFailed}, nil, nil)
	if strings.Contains(prompt, longCmd) {
		t.Error("prompt contains the full command, want it capped")
	}
	if !strings.Contains(prompt, "[truncated]") {
		t.Error("prompt missing the truncation note")
	}

	// Test 1: A short command is included whole with no truncation note.
	prompt = buildExplainPrompt(&run.Run{Tool: "bash", Command: "deploy.sh", Status: run.StatusFailed}, nil, nil)
	if !strings.Contains(prompt, "deploy.sh") || strings.Contains(prompt, "[truncated]") {
		t.Errorf("short command mishandled: %q", prompt)
	}

	// Test 2: A log tail cut mid-rune stays valid UTF-8.
	log := append(bytes.Repeat([]byte("é"), explainLogTail), []byte("tail end")...)
	prompt = buildExplainPrompt(&run.Run{Tool: "bash", Status: run.StatusFailed}, log, nil)
	if !utf8.ValidString(prompt) {
		t.Error("prompt is not valid UTF-8 after tail truncation")
	}
	if !strings.Contains(prompt, "tail end") {
		t.Error("prompt missing the log tail content")
	}
}

// TestBuildExplainPromptEvents covers the event context sections: failed tasks with rc and stderr,
// the cap preferring the most recent failures, the stats recap worst hosts first, and no sections
// when there are no events.
func TestBuildExplainPromptEvents(t *testing.T) {
	t.Parallel()
	rc := 2

	// Test 0: A failing task renders play, task, host, rc, message, and stderr.
	events := []event.Event{{
		Type: event.TypeRunnerFailed, Play: "site", Task: "apt update", Host: "web-1",
		RC: &rc, Message: "lock file held", Stderr: "E: could not get lock",
	}}
	prompt := buildExplainPrompt(&run.Run{Tool: "ansible", Status: run.StatusFailed}, nil, events)
	for _, want := range []string{"Failed tasks:", "site / apt update on web-1", "(rc=2)", "lock file held", "stderr: E: could not get lock"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}

	// Test 1: More failures than the cap keeps only the most recent ones.
	var many []event.Event
	for i := 0; i < explainMaxEvents+5; i++ {
		many = append(many, event.Event{
			Type: event.TypeRunnerFailed, Play: "p", Task: fmt.Sprintf("task-%d", i), Host: "h",
		})
	}
	prompt = buildExplainPrompt(&run.Run{Tool: "ansible", Status: run.StatusFailed}, nil, many)
	if strings.Contains(prompt, "task-0 ") || !strings.Contains(prompt, fmt.Sprintf("task-%d", explainMaxEvents+4)) {
		t.Errorf("failed task cap kept the wrong events:\n%s", prompt)
	}

	// Test 2: The stats recap lists the worst host first.
	stats := []event.Event{{
		Type: event.TypeStats,
		Stats: map[string]event.HostStats{
			"healthy": {OK: 8},
			"broken":  {OK: 2, Failures: 6},
		},
	}}
	prompt = buildExplainPrompt(&run.Run{Tool: "ansible", Status: run.StatusFailed}, nil, stats)
	broken, healthy := strings.Index(prompt, "- broken:"), strings.Index(prompt, "- healthy:")
	if broken < 0 || healthy < 0 || broken > healthy {
		t.Errorf("stats recap order wrong (broken=%d healthy=%d):\n%s", broken, healthy, prompt)
	}

	// Test 3: No events produce no event sections.
	prompt = buildExplainPrompt(&run.Run{Tool: "bash", Status: run.StatusFailed}, nil, nil)
	if strings.Contains(prompt, "Failed tasks:") || strings.Contains(prompt, "Host stats:") {
		t.Errorf("empty events grew sections:\n%s", prompt)
	}
}

// TestExplainRunIncludesEvents drives the handler end to end: a stored run with failing events
// produces a prompt that carries the failed task line.
func TestExplainRunIncludesEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{ID: "run_ev", Tool: "ansible", Status: run.StatusFailed}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.AppendEvents(ctx, "run_ev", []event.Event{{
		Type: event.TypeRunnerFailed, Play: "site", Task: "restart nginx", Host: "web-2", Message: "unit not found",
	}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}

	var sawTask bool
	provider := ai.ProviderFunc(func(_ context.Context, _, user string) (string, error) {
		sawTask = strings.Contains(user, "restart nginx on web-2")
		return "advice", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run_ev/explain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explain status = %d, want 200", rec.Code)
	}
	if !sawTask {
		t.Error("prompt did not include the failed task event")
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
