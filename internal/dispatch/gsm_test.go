package dispatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveGSMSecret(t *testing.T) {
	secretSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path != "/v1/projects/proj/secrets/ci/versions/latest:access" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		payload := base64.StdEncoding.EncodeToString([]byte("gsm-secret-value"))
		_ = json.NewEncoder(w).Encode(map[string]any{"payload": map[string]any{"data": payload}})
	}))
	defer secretSrv.Close()

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-tok"})
	}))
	defer metaSrv.Close()

	origEP, origMeta := gsmEndpoint, gsmMetadataEndpoint
	gsmEndpoint, gsmMetadataEndpoint = secretSrv.URL, metaSrv.URL
	defer func() { gsmEndpoint, gsmMetadataEndpoint = origEP, origMeta }()

	cfg := func(project, secret, token string) string {
		b, _ := json.Marshal(gsmConfig{Project: project, Secret: secret, Token: token})
		return string(b)
	}

	// Test 0: A config token reads the secret and decodes its base64 payload.
	if got, err := resolveGSMSecret(context.Background(), cfg("proj", "ci", "access-tok")); err != nil || got != "gsm-secret-value" {
		t.Errorf("with token = %q, %v; want gsm-secret-value", got, err)
	}
	// Test 1: With no config token, the metadata server supplies one.
	if got, err := resolveGSMSecret(context.Background(), cfg("proj", "ci", "")); err != nil || got != "gsm-secret-value" {
		t.Errorf("metadata token = %q, %v; want gsm-secret-value", got, err)
	}
	// Test 2: A missing project is an error.
	if _, err := resolveGSMSecret(context.Background(), cfg("", "ci", "access-tok")); !errors.Is(err, ErrSecretResolve) {
		t.Errorf("missing project error = %v, want ErrSecretResolve", err)
	}
	// Test 3: A bad token gets a non-200 and errors.
	if _, err := resolveGSMSecret(context.Background(), cfg("proj", "ci", "wrong")); !errors.Is(err, ErrSecretResolve) {
		t.Errorf("bad token error = %v, want ErrSecretResolve", err)
	}
	// Test 4: Invalid config JSON errors.
	if _, err := resolveGSMSecret(context.Background(), "{bad"); !errors.Is(err, ErrSecretResolve) {
		t.Errorf("bad config error = %v, want ErrSecretResolve", err)
	}
}
