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

	"go.uber.org/zap"

	"github.com/dcadolph/railwarden/internal/credential"
	"github.com/dcadolph/railwarden/internal/run"
	"github.com/dcadolph/railwarden/internal/template"
	"github.com/dcadolph/railwarden/internal/trigger"
)

// testSealer returns an enabled Sealer for trigger signing tests.
func testSealer() *credential.Sealer {
	return credential.NewSealer("test-pass", "test-salt")
}

// newTriggerServer builds a server handler with triggers, one template, and the given sealer.
func newTriggerServer(t *testing.T, sub Submitter, sealer *credential.Sealer) (http.Handler, trigger.Store) {
	t.Helper()
	triggers := trigger.NewMemStore()
	templates := template.NewMemStore()
	if err := templates.Save(context.Background(),
		&template.Template{ID: "tpl_1", Name: "deploy", Playbook: "site.yml"}); err != nil {
		t.Fatalf("save template: %v", err)
	}
	handler := New(run.NewMemStore(), sub, zap.NewNop(),
		WithTriggers(triggers, sealer), WithTemplates(templates)).Handler()
	return handler, triggers
}

// seedSignedTrigger stores a trigger with a sealed signing secret and returns the plaintext token
// and secret for signing hook requests.
func seedSignedTrigger(t *testing.T, triggers trigger.Store, sealer *credential.Sealer, require bool) (token, secret string) {
	t.Helper()
	token, tg, err := trigger.New("signed", "tpl_1")
	if err != nil {
		t.Fatalf("trigger.New: %v", err)
	}
	secret, err = trigger.NewSigningSecret()
	if err != nil {
		t.Fatalf("NewSigningSecret: %v", err)
	}
	sealed, err := sealer.Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tg.SigningSecret = sealed
	tg.RequireSignature = require
	if err := triggers.Save(context.Background(), tg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return token, secret
}

func TestCreateTriggerSigningSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name             string
		Body             string
		Sealer           *credential.Sealer
		WantStatus       int
		WantSecretPrefix string
		WantBodyContains string
	}{
		{ // Test 0: Encryption on mints and returns a signing secret.
			Name: "with encryption", Body: `{"name":"n","template_id":"tpl_1"}`,
			Sealer: testSealer(), WantStatus: http.StatusCreated, WantSecretPrefix: "whs_",
		},
		{ // Test 1: Encryption on with enforcement requested succeeds.
			Name: "require with encryption", Body: `{"name":"n","template_id":"tpl_1","require_signature":true}`,
			Sealer: testSealer(), WantStatus: http.StatusCreated, WantSecretPrefix: "whs_",
			WantBodyContains: `"require_signature":true`,
		},
		{ // Test 2: Enforcement requested without an encryption key is rejected.
			Name: "require without encryption", Body: `{"name":"n","template_id":"tpl_1","require_signature":true}`,
			Sealer: credential.NewSealer("", ""), WantStatus: http.StatusConflict,
			WantBodyContains: "require_signature needs an encryption key",
		},
		{ // Test 3: No encryption key still creates a trigger without a secret.
			Name: "no encryption", Body: `{"name":"n","template_id":"tpl_1"}`,
			Sealer: credential.NewSealer("", ""), WantStatus: http.StatusCreated,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler, _ := newTriggerServer(t, &fakeSubmitter{}, test.Sealer)
			req := httptest.NewRequest(http.MethodPost, "/v1/triggers", strings.NewReader(test.Body))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, test.WantStatus, rec.Body.String())
			}
			if test.WantBodyContains != "" && !strings.Contains(rec.Body.String(), test.WantBodyContains) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), test.WantBodyContains)
			}
			if test.WantSecretPrefix != "" {
				var resp createTriggerResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if !strings.HasPrefix(resp.SigningSecret, test.WantSecretPrefix) {
					t.Errorf("signing_secret = %q, want %s prefix", resp.SigningSecret, test.WantSecretPrefix)
				}
			}
		})
	}
}

func TestHookSignatureVerification(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main"}`)
	tests := []struct {
		Name       string
		Require    bool
		SignWith   string // "correct", "wrong", or "" for no header
		TamperBody bool
		WantStatus int
	}{
		{ // Test 0: A correct signature fires the trigger.
			Name: "valid signature", Require: true, SignWith: "correct", WantStatus: http.StatusAccepted,
		},
		{ // Test 1: A wrong signature is rejected.
			Name: "wrong signature", Require: true, SignWith: "wrong", WantStatus: http.StatusUnauthorized,
		},
		{ // Test 2: A missing signature is rejected when required.
			Name: "missing signature", Require: true, SignWith: "", WantStatus: http.StatusUnauthorized,
		},
		{ // Test 3: A correct signature over a tampered body is rejected.
			Name: "tampered body", Require: true, SignWith: "correct", TamperBody: true,
			WantStatus: http.StatusUnauthorized,
		},
		{ // Test 4: Without enforcement, an unsigned webhook still fires.
			Name: "not required", Require: false, SignWith: "", WantStatus: http.StatusAccepted,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sealer := testSealer()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}}
			handler, triggers := newTriggerServer(t, sub, sealer)
			token, secret := seedSignedTrigger(t, triggers, sealer, test.Require)

			sent := body
			req := httptest.NewRequest(http.MethodPost, "/hooks/"+token, bytes.NewReader(sent))
			switch test.SignWith {
			case "correct":
				signed := body
				if test.TamperBody {
					// Sign the original body but send a different one.
					req = httptest.NewRequest(http.MethodPost, "/hooks/"+token,
						bytes.NewReader([]byte(`{"ref":"refs/heads/evil"}`)))
				}
				req.Header.Set("X-Hub-Signature-256", trigger.SignBody(secret, signed))
			case "wrong":
				req.Header.Set("X-Hub-Signature-256", trigger.SignBody("whs_wrong", sent))
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

func TestRotateTriggerSecret(t *testing.T) {
	t.Parallel()
	sealer := testSealer()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}}
	handler, triggers := newTriggerServer(t, sub, sealer)
	token, oldSecret := seedSignedTrigger(t, triggers, sealer, true)

	// Rotate the secret and read the new plaintext.
	tg, err := triggers.FindByTokenHash(context.Background(), trigger.HashToken(token))
	if err != nil {
		t.Fatalf("find trigger: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/triggers/"+tg.ID+"/rotate-secret", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp rotateTriggerSecretResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if !strings.HasPrefix(resp.SigningSecret, "whs_") || resp.SigningSecret == oldSecret {
		t.Fatalf("new secret = %q, want fresh whs_ value", resp.SigningSecret)
	}

	body := []byte(`{"ref":"refs/heads/main"}`)
	// The new secret verifies.
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+token, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", trigger.SignBody(resp.SigningSecret, body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("new secret status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	// The old secret no longer verifies.
	req = httptest.NewRequest(http.MethodPost, "/hooks/"+token, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", trigger.SignBody(oldSecret, body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("old secret status = %d, want 401", rec.Code)
	}
}

func TestRotateTriggerSecretWithoutEncryption(t *testing.T) {
	t.Parallel()
	handler, triggers := newTriggerServer(t, &fakeSubmitter{}, credential.NewSealer("", ""))
	token, tg, err := trigger.New("n", "tpl_1")
	if err != nil {
		t.Fatalf("trigger.New: %v", err)
	}
	_ = token
	if err := triggers.Save(context.Background(), tg); err != nil {
		t.Fatalf("save trigger: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/triggers/"+tg.ID+"/rotate-secret", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateTriggerRequireSignature(t *testing.T) {
	t.Parallel()
	sealer := testSealer()
	handler, triggers := newTriggerServer(t, &fakeSubmitter{}, sealer)

	// A trigger with a secret can turn enforcement on.
	tokenA, _ := seedSignedTrigger(t, triggers, sealer, false)
	tgA, err := triggers.FindByTokenHash(context.Background(), trigger.HashToken(tokenA))
	if err != nil {
		t.Fatalf("find trigger: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/triggers/"+tgA.ID,
		strings.NewReader(`{"name":"renamed","require_signature":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err := triggers.Get(context.Background(), tgA.ID)
	if err != nil {
		t.Fatalf("get trigger: %v", err)
	}
	if got.Name != "renamed" || !got.RequireSignature {
		t.Errorf("trigger = %+v, want renamed and require_signature true", got)
	}

	// A trigger without a secret cannot turn enforcement on.
	_, tgB, err := trigger.New("no-secret", "tpl_1")
	if err != nil {
		t.Fatalf("trigger.New: %v", err)
	}
	if err := triggers.Save(context.Background(), tgB); err != nil {
		t.Fatalf("save trigger: %v", err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/triggers/"+tgB.ID,
		strings.NewReader(`{"name":"n","require_signature":true}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}
