package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveOnePassword exercises the 1Password Connect resolver against a mock Connect server,
// covering name-to-id resolution, id passthrough, field selection, and the error cases.
func TestResolveOnePassword(t *testing.T) {
	t.Parallel()
	const vaultID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const itemID = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	const noPassID = "cccccccccccccccccccccccccc"

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		filter := r.URL.Query().Get("filter")
		switch r.URL.Path {
		case "/v1/vaults":
			switch filter {
			case `name eq "Prod"`:
				_, _ = w.Write([]byte(`[{"id":"` + vaultID + `","name":"Prod"}]`))
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		case "/v1/vaults/" + vaultID + "/items":
			switch filter {
			case `title eq "DB"`:
				_, _ = w.Write([]byte(`[{"id":"` + itemID + `","title":"DB"}]`))
			case `title eq "NoPass"`:
				_, _ = w.Write([]byte(`[{"id":"` + noPassID + `","title":"NoPass"}]`))
			case `title eq "Dup"`:
				_, _ = w.Write([]byte(`[{"id":"x","title":"Dup"},{"id":"y","title":"Dup"}]`))
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		case "/v1/vaults/" + vaultID + "/items/" + itemID:
			_, _ = w.Write([]byte(`{"fields":[
				{"id":"pw","label":"password","value":"s3cr3t","purpose":"PASSWORD"},
				{"id":"un","label":"username","value":"svc"},
				{"id":"blank","label":"empty","value":""}
			]}`))
		case "/v1/vaults/" + vaultID + "/items/" + noPassID:
			_, _ = w.Write([]byte(`{"fields":[{"id":"n","label":"note","value":"x"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := func(c onePasswordConfig) string {
		c.URL = srv.URL
		b, _ := json.Marshal(c)
		return string(b)
	}

	// Test 0: Resolve vault and item by name, returning the password field, with a bearer token.
	got, err := resolveOnePassword(context.Background(), cfg(onePasswordConfig{
		Token: "conn-token", Vault: "Prod", Item: "DB",
	}))
	if err != nil || got != "s3cr3t" {
		t.Errorf("by name = %q, %v; want s3cr3t", got, err)
	}
	if gotAuth != "Bearer conn-token" {
		t.Errorf("auth = %q, want Bearer conn-token", gotAuth)
	}

	// Test 1: Connect ids are used directly, skipping the name lookups.
	if got, err := resolveOnePassword(context.Background(), cfg(onePasswordConfig{
		Token: "t", Vault: vaultID, Item: itemID,
	})); err != nil || got != "s3cr3t" {
		t.Errorf("by id = %q, %v; want s3cr3t", got, err)
	}

	// Test 2: A field label selects that field instead of the password.
	if got, err := resolveOnePassword(context.Background(), cfg(onePasswordConfig{
		Token: "t", Vault: "Prod", Item: "DB", Field: "username",
	})); err != nil || got != "svc" {
		t.Errorf("field = %q, %v; want svc", got, err)
	}

	// Test 3: Missing url, token, vault, or item errors before any request.
	if _, err := resolveOnePassword(context.Background(),
		`{"token":"t","vault":"v","item":"i"}`); !errors.Is(err, ErrResolve) {
		t.Errorf("missing url error = %v, want ErrResolve", err)
	}

	// Test 4: An unknown vault, an ambiguous item, a valued-less field, an unknown field, and an item
	// with no password field all error.
	errCfgs := map[string]onePasswordConfig{
		"unknown vault":  {Token: "t", Vault: "Ghost", Item: "DB"},
		"ambiguous item": {Token: "t", Vault: "Prod", Item: "Dup"},
		"empty field":    {Token: "t", Vault: "Prod", Item: "DB", Field: "empty"},
		"unknown field":  {Token: "t", Vault: "Prod", Item: "DB", Field: "nope"},
		"no password":    {Token: "t", Vault: "Prod", Item: "NoPass"},
	}
	for name, c := range errCfgs {
		if _, err := resolveOnePassword(context.Background(), cfg(c)); !errors.Is(err, ErrResolve) {
			t.Errorf("%s error = %v, want ErrResolve", name, err)
		}
	}

	// Test 5: Invalid config JSON errors.
	if _, err := resolveOnePassword(context.Background(), "{bad"); !errors.Is(err, ErrResolve) {
		t.Errorf("bad config error = %v, want ErrResolve", err)
	}
}
