package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheRelayMountAnnouncesThroughTheServer pins the wiring between the server and the relay it
// mounts. The relay announces what workers do only if the server hands it an announcer, and a
// server that took the option and dropped it would leave every worker's hold unannounced with every
// relay test still passing, since those build the relay handler themselves.
func TestTheRelayMountAnnouncesThroughTheServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{
		ID: "run_plan", Tool: run.ToolTerraform, Command: "infra/prod", Status: run.StatusRunning,
		CreatedAt: time.Now(), ClaimedBy: "worker-a", ClaimSecret: "capability-for-run_plan",
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	rules := policy.NewMemStore()
	gate := policy.NewPolicy("terraform destroys need a person")
	gate.Tool, gate.MaxDestroy = run.ToolTerraform, 0
	if err := rules.Save(ctx, gate); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	var mu sync.Mutex
	var said []string
	announcer := relay.AnnouncerFunc(func(r *run.Run) {
		mu.Lock()
		defer mu.Unlock()
		said = append(said, r.ID)
	})
	sealer := planSealerFunc(func(plan []byte) (string, error) { return "sealed:" + string(plan), nil })
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithRelay(store, "worker-token"),
		WithPolicies(rules), WithAnnouncer(announcer), WithPlanSealer(sealer)).Handler()

	req := httptest.NewRequest(http.MethodPost, "/relay/v1/runs/run_plan/propose-apply",
		strings.NewReader(`{"destroys":3,"read":true,"plan_file":"cGxhbg=="}`))
	req.Header.Set("Authorization", "Bearer worker-token")
	req.Header.Set("X-Switchtender-Lease", "capability-for-run_plan")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("propose through the server answered %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(said) != 1 {
		t.Fatalf("the server's relay announced %d runs, want the held apply once: %v", len(said), said)
	}
}

// planSealerFunc adapts a function to a relay plan sealer.
type planSealerFunc func(plan []byte) (string, error)

// SealPlanFile calls f.
func (f planSealerFunc) SealPlanFile(plan []byte) (string, error) { return f(plan) }
