package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
