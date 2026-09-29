package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTriggerWritesAuthorizeTheTemplateTheyFire checks that a webhook cannot be built around a
// template the caller may not use.
//
// A webhook fires a template with nobody present, and the hook itself carries no identity at fire
// time. That makes a trigger a durable re-entry point into the template behind it: it survives its
// author's demotion, and since firing does no authorization at all, there is nothing to revoke. So
// the check has to happen when the trigger is written. Schedules already did this for the same
// reason; triggers did not, so an operator refused a template could wrap it in a webhook and run it.
func TestTriggerWritesAuthorizeTheTemplateTheyFire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tpls := template.NewMemStore()
	if err := tpls.Save(ctx, &template.Template{ID: "tpl_prod", Name: "prod", Playbook: "prod.yml"}); err != nil {
		t.Fatalf("Save() template error = %v", err)
	}
	trs := trigger.NewMemStore()
	existing := &trigger.Trigger{ID: "trg_1", Name: "theirs", TemplateID: "tpl_prod"}
	if err := trs.Save(ctx, existing); err != nil {
		t.Fatalf("Save() trigger error = %v", err)
	}
	// Mallory holds nothing on tpl_prod.
	authz := &authorizer{strict: true, grants: &fakeGrants{byObject: map[string][]*grant.Grant{}}}
	actor := func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), actorKey{},
			Actor{UserID: "user_m", Role: user.RoleOperator}))
	}

	tests := []struct {
		Name    string
		Handler http.HandlerFunc
		Method  string
		Path    string
		Body    string
	}{{ // Test 0: Wrapping somebody else's template in a new webhook.
		Name:    "create",
		Handler: createTriggerHandler(trs, tpls, nil, authz, zap.NewNop()),
		Method:  http.MethodPost, Path: "/v1/triggers",
		Body: `{"name":"mine","template_id":"tpl_prod"}`,
	}, { // Test 1: Turning signature enforcement off on somebody else's webhook.
		Name:    "update",
		Handler: updateTriggerHandler(trs, tpls, authz, zap.NewNop()),
		Method:  http.MethodPut, Path: "/v1/triggers/trg_1",
		Body: `{"name":"theirs","require_signature":false}`,
	}, { // Test 2: Deleting somebody else's webhook, silently stopping a deployment path.
		Name:    "delete",
		Handler: deleteTriggerHandler(trs, authz, zap.NewNop()),
		Method:  http.MethodDelete, Path: "/v1/triggers/trg_1",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(test.Method, test.Path, strings.NewReader(test.Body))
			req.SetPathValue("id", "trg_1")
			rec := httptest.NewRecorder()
			test.Handler.ServeHTTP(rec, actor(req))
			if rec.Code < 400 {
				t.Errorf("%s answered %d for a template the caller may not use, so a webhook "+
					"becomes a way around the grant: %s", test.Name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHookProbeDoesNotAppendToTheChain checks that guessing a webhook token cannot fill the audit
// chain. The path is the credential, so anyone on the network can present a guess.
func TestHookProbeDoesNotAppendToTheChain(t *testing.T) {
	t.Parallel()
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
	} {
		req := httptest.NewRequest(method, "/hooks/whk_livetokenlivetokenlivetoken", nil)
		if got := auditPath(req); strings.Contains(got, "whk_livetoken") {
			t.Errorf("%s records %q, embedding a live webhook token in a hash-linked chain that "+
				"travels in every bundle handed to a third party", method, got)
		}
	}
	// Odd spellings reach the same handler and must redact the same way.
	for _, p := range []string{"//hooks/whk_livetoken", "/HOOKS/whk_livetoken", "/v1/hooks/whk_livetoken"} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		if got := auditPath(req); strings.Contains(got, "whk_livetoken") {
			t.Errorf("%s records %q", p, got)
		}
	}
}

// TestHookEndpointIsRateLimited checks that an endpoint reachable without a credential cannot be
// probed without bound.
//
// A wrong token answers 404 and a right token with a bad signature answers 401, so a caller can
// confirm a live webhook token without the signing secret. That is a usable oracle, and it ran at
// unlimited rate: two thousand probes completed in under six milliseconds. The window is per client
// address so one noisy sender cannot stop another's deliveries.
func TestHookEndpointIsRateLimited(t *testing.T) {
	t.Parallel()
	handler := hookHandler(trigger.NewMemStore(), template.NewMemStore(), &fakeSubmitter{},
		nil, nil, nil, zap.NewNop())

	limited := false
	for i := 0; i < hookWindowMax+20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/hooks/whk_guess", nil)
		req.RemoteAddr = "203.0.113.9:5000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("%d probes from one address were all answered, so a live webhook token can be "+
			"confirmed by sweeping at full speed", hookWindowMax+20)
	}

	// A different sender is unaffected, so one noisy client cannot stop another's deliveries.
	req := httptest.NewRequest(http.MethodPost, "/hooks/whk_guess", nil)
	req.RemoteAddr = "198.51.100.4:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Error("a second sender was refused because the first was noisy")
	}
}
