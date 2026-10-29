package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestACommunityRelayMountRefusesNamingTeam pins what a Community build answers on the worker
// endpoints when a server is configured for workers anyway: the relay code is not in the binary, so
// every relay path answers 403 with the Team line and the rest of the API is untouched.
func TestACommunityRelayMountRefusesNamingTeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path       string
		WantStatus int
		WantBody   string
	}{{ // Test 0: A worker's claim is refused naming Team.
		Path: "/relay/v1/claim", WantStatus: http.StatusForbidden,
		WantBody: "A Team license is required for running distributed workers",
	}, { // Test 1: A worker reading policies is refused the same way.
		Path: "/relay/v1/policies", WantStatus: http.StatusForbidden,
		WantBody: "A Team license is required for running distributed workers",
	}, { // Test 2: The API itself still answers.
		Path: "/healthz", WantStatus: http.StatusOK,
	}}
	store := run.NewMemStore()
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithRelay(store, "worker-token")).Handler()
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, test.Path, strings.NewReader(`{}`))
			if test.Path == "/healthz" {
				req = httptest.NewRequest(http.MethodGet, test.Path, nil)
			}
			req.Header.Set("Authorization", "Bearer worker-token")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if diff := cmp.Diff(test.WantStatus, rec.Code); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantBody) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), test.WantBody)
			}
		})
	}
}

// TestTheRelayMountHandsTheEngineWhatTheServerHolds pins the wiring between the server and the
// relay engine it mounts. The engine announces, seals plan files, admits proposals, and serves
// policies only with what the server hands it, and a server that took an option and dropped it would
// leave a worker's hold unannounced or its proposal unchecked with every relay test still passing.
func TestTheRelayMountHandsTheEngineWhatTheServerHolds(t *testing.T) {
	t.Parallel()
	errFull := errors.New("disk full")
	store := run.NewMemStore()
	rules := policy.NewMemStore()
	var announced []string
	announcer := relay.AnnouncerFunc(func(r *run.Run) { announced = append(announced, r.ID) })
	sealer := mountSealer(func(plan []byte) (string, error) { return "sealed:" + string(plan), nil })
	engine := &recordingEngine{}
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithRelay(store, "worker-token"),
		WithRelayEngine(engine), WithPolicies(rules), WithAnnouncer(announcer), WithPlanSealer(sealer),
		WithSecretOpener(mountOpener{}), WithRelayAdmission(func() error { return errFull })).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/relay/v1/claim", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("relay path answered %d, want the engine's handler (418)", rec.Code)
	}
	cfg := engine.cfg
	if diff := cmp.Diff("worker-token", engine.token); diff != "" {
		t.Errorf("single pool token mismatch (-want +got):\n%s", diff)
	}
	if cfg.Store != store || cfg.Policies != rules || cfg.PlanSealer == nil || cfg.Pools == nil ||
		cfg.SecretOpener == nil {
		t.Errorf("handler config dropped the store, policies, sealer, opener, or pools: %+v", cfg)
	}
	if cfg.Admit == nil || !errors.Is(cfg.Admit(), errFull) {
		t.Errorf("handler config did not carry the server's admission")
	}
	if cfg.Announcer == nil {
		t.Fatal("handler config dropped the announcer")
	}
	cfg.Announcer.Announce(&run.Run{ID: "run_held"})
	if diff := cmp.Diff([]string{"run_held"}, announced); diff != "" {
		t.Errorf("announcer mismatch (-want +got):\n%s", diff)
	}
}

// recordingEngine is a relay engine that records what the server hands it and answers every relay
// path with 418, so a test can tell its handler from a refusal.
type recordingEngine struct {
	// token is the worker token SinglePool was given.
	token string
	// cfg is the configuration Handler was given.
	cfg relay.HandlerConfig
}

// LoadPools returns a pool set.
func (e *recordingEngine) LoadPools(string) (relay.Pools, error) { return recordedPools{}, nil }

// SinglePool records the token.
func (e *recordingEngine) SinglePool(token string) (relay.Pools, error) {
	e.token = token
	return recordedPools{}, nil
}

// Handler records cfg.
func (e *recordingEngine) Handler(cfg relay.HandlerConfig) (http.Handler, error) {
	e.cfg = cfg
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), nil
}

// Dial refuses.
func (e *recordingEngine) Dial(relay.DialConfig) (*relay.Worker, error) {
	return nil, license.ErrRequiresTier
}

// recordedPools is the pool set a recordingEngine hands out.
type recordedPools struct{}

// DeliveryKeyIDs returns none.
func (recordedPools) DeliveryKeyIDs() map[string]string { return map[string]string{} }

// mountSealer adapts a function to a relay plan sealer.
type mountSealer func(plan []byte) (string, error)

// SealPlanFile calls f.
func (f mountSealer) SealPlanFile(plan []byte) (string, error) { return f(plan) }

// mountOpener is a relay secret opener that opens nothing.
type mountOpener struct{}

// NeedsSecrets reports none.
func (mountOpener) NeedsSecrets(context.Context, *run.Run) bool { return false }

// OpenSecrets opens nothing.
func (mountOpener) OpenSecrets(context.Context, *run.Run) (*handoff.Payload, func(), error) {
	return nil, nil, nil
}
