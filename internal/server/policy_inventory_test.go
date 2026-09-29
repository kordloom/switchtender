package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestPolicyMaxDestroyDefaults verifies the destroy guard is off unless a policy asks for it, and
// that an explicit zero means "hold on any destroy" rather than being mistaken for absent. Getting
// this wrong would either gate every Terraform apply or silently let destructive plans through.
func TestPolicyMaxDestroyDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body string
		Want int
	}{
		{Name: "absent", Body: `{"name":"any tf","tool":"terraform"}`, Want: policy.DisabledMaxDestroy},
		{Name: "zero holds any destroy", Body: `{"name":"strict","tool":"terraform","max_destroy":0}`, Want: 0},
		{Name: "threshold", Body: `{"name":"loose","tool":"terraform","max_destroy":5}`, Want: 5},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := policy.NewMemStore()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithPolicies(store)).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
				strings.NewReader(test.Body)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("%s: create = %d, body %s", test.Name, rec.Code, rec.Body.String())
			}
			list, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("%s: stored %d policies, want 1", test.Name, len(list))
			}
			if list[0].MaxDestroy != test.Want {
				t.Errorf("%s: MaxDestroy = %d, want %d", test.Name, list[0].MaxDestroy, test.Want)
			}
		})
	}
}

// TestPolicyRejectsUnknownTool verifies a policy cannot name a tool the engine does not run, which
// would create a rule that silently never matches.
func TestPolicyRejectsUnknownTool(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithPolicies(policy.NewMemStore())).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
		strings.NewReader(`{"name":"bad","tool":"cobol"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown tool = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
		strings.NewReader(`{"tool":"terraform"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing name = %d, want 400", rec.Code)
	}
}

// TestInventoryContentSources verifies the rules around externally sourced inventory content: the
// source must be recognized, local content is required for a local inventory, and an external
// source is refused without encryption rather than storing its configuration in the clear.
func TestInventoryContentSources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Body       string
		Sealed     bool
		WantStatus int
	}{
		{
			Name: "local needs content", Body: `{"name":"inv","content_source":"local"}`,
			WantStatus: http.StatusBadRequest,
		},
		{
			Name: "local stores content", Body: `{"name":"inv","content":"[all]\nweb01\n"}`,
			WantStatus: http.StatusCreated,
		},
		{
			Name: "unknown source refused", Body: `{"name":"inv","content_source":"carrier-pigeon"}`,
			WantStatus: http.StatusBadRequest,
		},
		{
			Name:       "external source without encryption refused",
			Body:       `{"name":"inv","content_source":"vault","content_config":"secret/data/hosts"}`,
			WantStatus: http.StatusConflict,
		},
		{
			Name:       "external source with encryption accepted",
			Body:       `{"name":"inv","content_source":"vault","content_config":"secret/data/hosts"}`,
			Sealed:     true,
			WantStatus: http.StatusCreated,
		},
		{
			Name: "external source needs a config", Body: `{"name":"inv","content_source":"command"}`,
			Sealed: true, WantStatus: http.StatusBadRequest,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := inventory.NewMemStore()
			// The sealer reaches the inventory handlers through the credentials option, which is
			// where the server takes it.
			opts := []Option{WithInventories(store)}
			if test.Sealed {
				opts = append(opts, WithCredentials(credential.NewMemStore(),
					credential.NewSealer("test-key-material", "test-salt-material")))
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), opts...).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/inventories",
				strings.NewReader(test.Body)))
			if rec.Code != test.WantStatus {
				t.Fatalf("%s = %d, want %d, body %s",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
			if rec.Code != http.StatusCreated {
				return
			}
			// A sealed source must never echo its configuration back in the clear.
			if strings.Contains(rec.Body.String(), "secret/data/hosts") {
				t.Errorf("%s: response leaked the raw content config: %s", test.Name, rec.Body.String())
			}
		})
	}
}
