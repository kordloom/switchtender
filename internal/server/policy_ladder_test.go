package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestThePolicyAPIEnforcesTheLadder holds both policy handlers to the ladder from each side of the
// Pro line. Deny rules and the risk and reversibility floors save on Pro and are refused on
// Community naming Pro. Separation of duties and actor scoping are refused on Pro naming Team. An
// exemption naming an account saves on Community, and an exemption carrying a floor is refused as
// invalid, not sold: Validate refuses it on every tier. A lapsed license refuses to author, since
// what a lapse keeps is enforcement. Not parallel: the license is process state.
func TestThePolicyAPIEnforcesTheLadder(t *testing.T) {
	held := license.Current()
	t.Cleanup(func() { license.Set(held) })
	pro := func(expires time.Duration) *license.License {
		return &license.License{Claims: license.Claims{
			V: 1, ID: "lic_pro", Org: "Acme", Tier: license.TierPro, Hosts: "500",
			Issued:  time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
			Expires: time.Now().Add(expires).Format(time.RFC3339),
		}}
	}
	tests := []struct {
		// License is the license in force, nil for Community.
		License *license.License
		// Method is POST to create a rule or PUT to edit a plain one into it.
		Method string
		// Body is the request body.
		Body string
		// WantCode is the status the handler answers.
		WantCode int
		// WantText is a phrase the answer carries, empty for none.
		WantText string
	}{{ // Test 0: A deny rule on Pro.
		License: pro(24 * time.Hour), Method: http.MethodPost,
		Body: `{"name":"deny prod","effect":"deny"}`, WantCode: http.StatusCreated,
	}, { // Test 1: A risk floor on Pro.
		License: pro(24 * time.Hour), Method: http.MethodPost,
		Body: `{"name":"hold risky","min_risk":"high"}`, WantCode: http.StatusCreated,
	}, { // Test 2: A reversibility floor on Pro.
		License: pro(24 * time.Hour), Method: http.MethodPost,
		Body: `{"name":"hold forever","reversibility":"irreversible"}`, WantCode: http.StatusCreated,
	}, { // Test 3: All three in one rule on Pro.
		License: pro(24 * time.Hour), Method: http.MethodPost,
		Body:     `{"name":"all","effect":"deny","min_risk":"medium","reversibility":"costly"}`,
		WantCode: http.StatusCreated,
	}, { // Test 4: A plain rule edited into a deny rule with a risk floor on Pro.
		License: pro(24 * time.Hour), Method: http.MethodPut,
		Body: `{"name":"hold prod","effect":"deny","min_risk":"high"}`, WantCode: http.StatusOK,
	}, { // Test 5: Separation of duties on Pro names Team.
		License: pro(24 * time.Hour), Method: http.MethodPost,
		Body:     `{"name":"two people","require_distinct_approver":true}`,
		WantCode: http.StatusForbidden,
		WantText: "A Team license is required for the full policy engine; this install's Pro " +
			"license does not include it",
	}, { // Test 6: Scoping to agents, edited in on Pro, names Team.
		License: pro(24 * time.Hour), Method: http.MethodPut,
		Body:     `{"name":"hold prod","actor_kind":"agent"}`,
		WantCode: http.StatusForbidden,
		WantText: "A Team license is required for the full policy engine; this install's Pro " +
			"license does not include it",
	}, { // Test 7: A risk floor on Community names Pro.
		Method: http.MethodPost, Body: `{"name":"hold risky","min_risk":"high"}`,
		WantCode: http.StatusForbidden,
		WantText: "A Pro license is required for deny rules and risk and reversibility floors; " +
			"this install runs Community",
	}, { // Test 8: A reversibility floor on Community names Pro.
		Method: http.MethodPost, Body: `{"name":"hold forever","reversibility":"irreversible"}`,
		WantCode: http.StatusForbidden,
		WantText: "A Pro license is required for deny rules and risk and reversibility floors; " +
			"this install runs Community",
	}, { // Test 9: A plain rule edited into a deny rule on Community names Pro.
		Method: http.MethodPut, Body: `{"name":"hold prod","effect":"deny"}`,
		WantCode: http.StatusForbidden,
		WantText: "A Pro license is required for deny rules and risk and reversibility floors; " +
			"this install runs Community",
	}, { // Test 10: An exemption naming an account on Community.
		Method: http.MethodPost, Body: `{"name":"ci","effect":"exempt","account":"team-a"}`,
		WantCode: http.StatusCreated,
	}, { // Test 11: An exemption carrying a risk floor is invalid on every tier.
		Method: http.MethodPost, Body: `{"name":"ci","effect":"exempt","min_risk":"high"}`,
		WantCode: http.StatusBadRequest, WantText: "an exemption takes only",
	}, { // Test 12: A deny rule on a lapsed Pro is not authored.
		License: pro(-time.Hour), Method: http.MethodPost,
		Body: `{"name":"deny prod","effect":"deny"}`, WantCode: http.StatusForbidden,
		WantText: "lapsed",
	}}
	for testNum, test := range tests {
		store := policy.NewMemStore()
		handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
			WithPolicies(store)).Handler()
		path := "/v1/policies"
		if test.Method == http.MethodPut {
			plain := policy.NewPolicy("hold prod")
			if err := store.Save(context.Background(), plain); err != nil {
				t.Fatalf("test %d: save the plain rule: %v", testNum, err)
			}
			path += "/" + plain.ID
		}
		license.Set(test.License)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(test.Method, path, strings.NewReader(test.Body)))
		if rec.Code != test.WantCode {
			t.Errorf("test %d: %s %s = %d, want %d: %s", testNum, test.Method, test.Body, rec.Code,
				test.WantCode, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), test.WantText) {
			t.Errorf("test %d: answer = %s, want it to contain %q", testNum, rec.Body.String(),
				test.WantText)
		}
	}
}
