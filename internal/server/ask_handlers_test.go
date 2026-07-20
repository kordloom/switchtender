package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/ai"
	"github.com/dcadolph/switchtender/internal/run"
)

// TestAskFleet covers the fleet question endpoint: disabled 404, empty question 400, the happy
// path carrying a deterministic snapshot with recent runs, health, and drift, and the provider
// failure mapping to a generic 502.
func TestAskFleet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &run.Run{
		ID: "run_a", Playbook: "site.yml", Status: run.StatusFailed, CreatedAt: at,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, "run_a", []run.HostSummary{
		{Host: "web01", Failures: 1, Worst: "failed", RanAt: at},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	seedDrift(t, store, "chk_w", "", "web02", 4)

	// Test 0: With no provider, the endpoint is disabled.
	off := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/ask",
		strings.NewReader(`{"question":"what is failing"}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("ask with no provider status = %d, want 404", rec.Code)
	}

	var gotUser string
	provider := ai.ProviderFunc(func(_ context.Context, _, user string) (string, error) {
		gotUser = user
		return "web01 failed its last run and web02 has drifted.", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()

	// Test 1: An empty question is refused.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/ask",
		strings.NewReader(`{"question":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty question status = %d, want 400", rec.Code)
	}

	// Test 2: A question reaches the provider with the snapshot and comes back answered.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/ask",
		strings.NewReader(`{"question":"which hosts need attention"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("ask status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"run_a", "web01", "web02: 4 drifted tasks", "Question: which hosts need attention",
	} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("snapshot missing %q:\n%s", want, gotUser)
		}
	}
	var body struct {
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Answer, "web01") {
		t.Errorf("answer = %q, want the model reply", body.Answer)
	}

	// Test 3: A provider failure maps to a generic 502 without internals.
	boom := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "", context.DeadlineExceeded
	})
	broken := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(boom)).Handler()
	rec = httptest.NewRecorder()
	broken.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/ask",
		strings.NewReader(`{"question":"anything"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("provider failure status = %d, want 502", rec.Code)
	}
}

// TestAskFleetRateLimit proves the fixed window refuses questions past the per-minute budget.
func TestAskFleetRateLimit(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	provider := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "ok", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()

	for i := 0; i < askRateLimit; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/ask",
			strings.NewReader(`{"question":"q"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("ask %d status = %d, want 200", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/ask",
		strings.NewReader(`{"question":"q"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("over-budget ask status = %d, want 429", rec.Code)
	}
}

// TestAskLimiterWindowReset proves a spent window reopens after a minute.
func TestAskLimiterWindowReset(t *testing.T) {
	t.Parallel()
	l := &askLimiter{}
	for i := 0; i < askRateLimit; i++ {
		if !l.allow() {
			t.Fatalf("allow() %d = false inside the budget", i)
		}
	}
	if l.allow() {
		t.Fatal("allow() = true past the budget")
	}
	l.windowStart = time.Now().Add(-2 * time.Minute)
	if !l.allow() {
		t.Fatal("allow() = false after the window reset")
	}
}
