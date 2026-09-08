package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestFirstPolicyRequiringADistinctApproverIsTeam covers the create path for the separation-of-duties
// flag. The policy count gate lets a Community install create its first policy, so nothing but the
// feature gate stands between a free install and the full engine here. Only the deny effect was
// covered before, and it was covered through update, so the create path could stop naming
// require_distinct_approver as advanced and every test still passed.
func TestFirstPolicyRequiringADistinctApproverIsTeam(t *testing.T) {
	store := policy.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithPolicies(store)).Handler()
	const body = `{"name":"two people","require_distinct_approver":true}`

	asCommunity(t, func() {
		// Test 0: the first policy is free, but requiring a second approver is not.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
			strings.NewReader(body)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("create with require_distinct_approver = %d, want 403: %s",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Team license") {
			t.Errorf("refusal does not name the tier: %s", rec.Body.String())
		}

		// Test 1: the refusal changed nothing, so no rule is enforcing unpaid.
		list, err := store.List(context.Background())
		if err != nil {
			t.Fatalf("list policies: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("a refused create stored %d policies, want 0", len(list))
		}
	})

	// Test 2: with the Team license back the same body saves, so the gate is the only thing that
	// refused it.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
		strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("licensed create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// TestFirstPolicyWithARiskFloorIsTeam covers the create path for the risk floor, the other criterion
// that turns a plain hold into the full engine. It is the same gate as the distinct-approver flag and
// was equally uncovered on create.
func TestFirstPolicyWithARiskFloorIsTeam(t *testing.T) {
	store := policy.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithPolicies(store)).Handler()
	const body = `{"name":"hold risky","min_risk":"high"}`

	asCommunity(t, func() {
		// Test 0: a risk floor is the full engine even as the first policy.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
			strings.NewReader(body)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("create with min_risk = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Team license") {
			t.Errorf("refusal does not name the tier: %s", rec.Body.String())
		}

		// Test 1: nothing was stored, so the risk floor is not quietly in force.
		list, err := store.List(context.Background())
		if err != nil {
			t.Fatalf("list policies: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("a refused create stored %d policies, want 0", len(list))
		}
	})

	// Test 2: the same body saves once the license is back.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
		strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("licensed create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}
