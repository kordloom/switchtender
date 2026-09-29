package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/run"
)

// TestProposeRun covers the propose endpoint: disabled 404, empty intent 400, a valid proposal
// held for approval and stamped with the intent, an unusable model reply 422, and a provider
// failure 502.
func TestProposeRun(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()

	// Test 0: With no provider, the endpoint is disabled.
	off := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
		strings.NewReader(`{"intent":"restart nginx"}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("propose with no provider status = %d, want 404", rec.Code)
	}

	// A model that returns a fenced JSON run, to exercise the fence stripping too.
	good := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "```json\n{\"tool\":\"bash\",\"command\":\"systemctl restart nginx\"," +
			"\"limit\":\"web-*\",\"dry_run\":false,\"summary\":\"Restart nginx\"}\n```", nil
	})
	fake := &fakeSubmitter{run: &run.Run{ID: "run_prop", Status: run.StatusPendingApproval}}
	handler := New(store, fake, zap.NewNop(), WithAI(good)).Handler()

	// Test 1: An empty intent is refused.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
		strings.NewReader(`{"intent":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty intent status = %d, want 400", rec.Code)
	}

	// Test 2: A valid reply builds a held proposal stamped with the intent.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
		strings.NewReader(`{"intent":"restart nginx on the web hosts"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("propose status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	got := fake.gotRun
	if got.Tool != "bash" || got.Command != "systemctl restart nginx" {
		t.Errorf("proposed run = %+v, want the generated bash command", got)
	}
	if got.Limit != "web-*" || got.DryRun {
		t.Errorf("proposed run limit/mode wrong: %+v", got)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("status = %q, want pending_approval", got.Status)
	}
	if got.Intent != "restart nginx on the web hosts" {
		t.Errorf("intent = %q, want the request stamped", got.Intent)
	}
	var resp run.Run
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "run_prop" {
		t.Errorf("response run = %q, want the submitted proposal", resp.ID)
	}

	// Test 3: A reply that is not a usable run is unprocessable.
	junk := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "I cannot help with that.", nil
	})
	badHandler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(junk)).Handler()
	rec = httptest.NewRecorder()
	badHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
		strings.NewReader(`{"intent":"do a thing"}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unusable reply status = %d, want 422", rec.Code)
	}

	// Test 4: A provider failure maps to a generic 502.
	boom := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "", context.DeadlineExceeded
	})
	broken := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(boom)).Handler()
	rec = httptest.NewRecorder()
	broken.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
		strings.NewReader(`{"intent":"anything"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("provider failure status = %d, want 502", rec.Code)
	}
}

// TestParseProposedRun covers the model-reply validation: a valid bash run, a valid ansible run,
// an unknown tool, a bash run missing its command, an ansible run missing its playbook, and
// unparseable text.
func TestParseProposedRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		OK   bool
		Tool string
	}{{ // Test 0: A valid bash run.
		Name: "bash", In: `{"tool":"bash","command":"echo hi"}`, OK: true, Tool: "bash",
	}, { // Test 1: A valid ansible run, command cleared.
		Name: "ansible", In: `{"tool":"ansible","playbook":"site.yml","command":"ignored"}`, OK: true, Tool: "ansible",
	}, { // Test 2: An unknown tool is rejected.
		Name: "unknown tool", In: `{"tool":"rm","command":"rm -rf /"}`, OK: false,
	}, { // Test 3: A bash run without a command is rejected.
		Name: "bash no command", In: `{"tool":"bash"}`, OK: false,
	}, { // Test 4: An ansible run without a playbook is rejected.
		Name: "ansible no playbook", In: `{"tool":"ansible"}`, OK: false,
	}, { // Test 5: Prose that is not JSON is rejected.
		Name: "not json", In: "I cannot do that", OK: false,
	}, { // Test 6: JSON embedded in prose is still parsed.
		Name: "embedded", In: `Here you go: {"tool":"go","command":"package main"} enjoy`, OK: true, Tool: "go",
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseProposedRun(test.In)
			if ok != test.OK {
				t.Fatalf("test %d: parseProposedRun ok = %v, want %v", testNum, ok, test.OK)
			}
			if ok && got.Tool != test.Tool {
				t.Errorf("test %d: tool = %q, want %q", testNum, got.Tool, test.Tool)
			}
			if ok && run.NormalizeTool(got.Tool) == run.ToolAnsible && got.Command != "" {
				t.Errorf("test %d: ansible run kept a command %q", testNum, got.Command)
			}
		})
	}
}

// TestExplainIntentProposal proves a held run proposed from a request is explainable, and the
// prompt carries the request and the generated command under the intent reviewer prompt.
func TestExplainIntentProposal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{
		ID: "run_i", Tool: "bash", Command: "systemctl restart nginx", Limit: "web-*",
		Status: run.StatusPendingApproval, Intent: "restart nginx on the web hosts",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	var gotSystem, gotUser string
	provider := ai.ProviderFunc(func(_ context.Context, system, user string) (string, error) {
		gotSystem, gotUser = system, user
		return "This restarts nginx on the web hosts, matching the request.", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_i/explain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explain intent proposal status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotSystem, "proposed from a plain-language request") {
		t.Errorf("system prompt = %q, want the intent reviewer framing", gotSystem)
	}
	for _, want := range []string{
		"restart nginx on the web hosts", "systemctl restart nginx", "web-*", "applies changes",
	} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("prompt missing %q:\n%s", want, gotUser)
		}
	}
}

// TestProposedRunCarriesTheRequestersAccount pins that an AI proposal records who asked for it, not
// only what they are called.
//
// Separation of duties compares accounts: sameActor prefers UserID and falls back to the display name
// only when one side has no account. A proposal that stored a name alone therefore let its own
// requester release it, because the fallback compared the browser session's username against the
// token label the run was created under and those never match. That is the ordinary arrangement, an
// admin holding a CI token and a browser session, and it left the one control between a generated
// command and production not applying at all.
func TestProposedRunCarriesTheRequestersAccount(t *testing.T) {
	t.Parallel()
	good := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return `{"tool":"bash","command":"systemctl restart nginx","summary":"Restart nginx"}`, nil
	})
	fake := &fakeSubmitter{run: &run.Run{ID: "run_prop", Status: run.StatusPendingApproval}}
	handler := New(run.NewMemStore(), fake, zap.NewNop(), WithAI(good)).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
		strings.NewReader(`{"intent":"restart nginx"}`))
	req = req.WithContext(context.WithValue(req.Context(), actorKey{},
		Actor{UserID: "usr_alice", Name: "alice-ci", Type: "token"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("propose status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if fake.gotRun.ActorUserID != "usr_alice" {
		t.Errorf("proposed run ActorUserID = %q, want %q; without it the distinct-approver check "+
			"falls back to name comparison and the requester can release their own proposal",
			fake.gotRun.ActorUserID, "usr_alice")
	}
}
