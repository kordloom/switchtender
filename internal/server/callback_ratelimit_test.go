package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// newObservedCallbackFixture is newCallbackFixture with a logger whose entries the test reads.
func newObservedCallbackFixture(t *testing.T, opts ...Option) (*callbackFixture, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := context.Background()
	store := run.NewMemStore()
	templates := template.NewMemStore()
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: callbackInventory, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := templates.Save(ctx, &template.Template{ID: "tpl_cb", Name: "boot", Playbook: "boot.yml",
		InventoryID: "inv_1", AllowCallbacks: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save template: %v", err)
	}
	audits := &recordingAudits{}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	all := append([]Option{
		WithTemplates(templates), WithInventories(inventories), WithAudit(audits),
		WithCredentials(credential.NewMemStore(), credential.NewSealer("pass", "salt")),
		func(s *Server) { s.callbackResolver = fakeResolver{} },
	}, opts...)
	return &callbackFixture{handler: New(store, sub, zap.New(core), all...).Handler(),
		templates: templates, store: store, audits: audits}, logs
}

// TestCallbackRateLimitsAreConfigurable pins decision eighteen. Both per-address limits take the
// values the server was given, a refusal names the limit and the flag that sets it and says when to
// try again, and the refusal is logged once for the address rather than once per request.
func TestCallbackRateLimitsAreConfigurable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Calls     func(f *callbackFixture, key string)
		WantBody  []string
		WantFlag  string
		PerMinute int
		WrongKeys int
	}{{ // Test 0: The callback budget is the one configured, and the refusal names its flag.
		PerMinute: 2, WrongKeys: 10, WantFlag: "--callback-rate-limit",
		WantBody: []string{"too many callbacks", "the limit is 2 a minute", "--callback-rate-limit"},
		Calls: func(f *callbackFixture, key string) {
			f.code("10.0.0.50", key)
			f.code("10.0.0.50", key)
		},
	}, { // Test 1: The wrong-key budget is the one configured, and the refusal names its flag.
		PerMinute: 30, WrongKeys: 1, WantFlag: "--callback-key-failure-limit",
		WantBody: []string{"too many wrong host config keys", "the limit is 1 a minute",
			"--callback-key-failure-limit"},
		Calls: func(f *callbackFixture, _ string) { f.code("10.0.0.50", "hck_guess") },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f, logs := newObservedCallbackFixture(t, WithCallbackRateLimits(test.PerMinute,
				test.WrongKeys))
			key := f.mintKey(t, "tpl_cb", nil)
			test.Calls(f, key)
			for range 3 {
				rec := f.callBack("tpl_cb", "10.0.0.50", "application/json", keyBody(key))
				if rec.Code != http.StatusTooManyRequests {
					t.Fatalf("status = %d body %s, want 429", rec.Code, rec.Body.String())
				}
				for _, want := range test.WantBody {
					if !strings.Contains(rec.Body.String(), want) {
						t.Errorf("refusal %s does not say %q", rec.Body.String(), want)
					}
				}
				if rec.Header().Get("Retry-After") != "60" {
					t.Errorf("Retry-After = %q, want 60", rec.Header().Get("Retry-After"))
				}
			}
			refusals := logs.FilterMessage("server: provisioning callback refused by a rate limit")
			if refusals.Len() != 1 {
				t.Fatalf("three refusals logged %d lines, want one per address and window",
					refusals.Len())
			}
			fields := refusals.All()[0].ContextMap()
			if fields["flag"] != test.WantFlag || fields["address"] != "10.0.0.50" {
				t.Errorf("logged %v, want the flag %s and the address", fields, test.WantFlag)
			}
			// Another address has a budget of its own.
			if code := f.code("10.0.0.11", key); code != http.StatusCreated {
				t.Errorf("another address: status = %d, want 201", code)
			}
		})
	}
}

// TestCallbackRateLimitDefaults pins the defaults a server applies when it is given nothing, and
// that a value below one keeps the default rather than refusing every callback or none.
func TestCallbackRateLimitDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Opts          []Option
		WantPerMinute int
		WantWrongKeys int
	}{{ // Test 0: Nothing configured.
		WantPerMinute: 30, WantWrongKeys: 10,
	}, { // Test 1: Zero keeps the defaults.
		Opts: []Option{WithCallbackRateLimits(0, 0)}, WantPerMinute: 30, WantWrongKeys: 10,
	}, { // Test 2: Values given are used.
		Opts: []Option{WithCallbackRateLimits(500, 3)}, WantPerMinute: 500, WantWrongKeys: 3,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			c := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), test.Opts...).newCallbacks()
			if c.perMinute != test.WantPerMinute || c.wrongKeys != test.WantWrongKeys {
				t.Errorf("limits %d and %d, want %d and %d", c.perMinute, c.wrongKeys,
					test.WantPerMinute, test.WantWrongKeys)
			}
		})
	}
}
