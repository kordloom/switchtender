package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// asCommunity runs body with no license installed and restores the package's Team license after,
// whatever happens. Deliberately not parallel: the license is process state.
func asCommunity(t *testing.T, body func()) {
	t.Helper()
	team := license.Current()
	license.Set(nil)
	defer license.Set(team)
	body()
}

// TestGatesRefuseInOneLineOnCommunity covers what a free install sees at each server gate: one
// line, the tier, the destination, and a refusal that changed nothing.
func TestGatesRefuseInOneLineOnCommunity(t *testing.T) {
	store := policy.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithPolicies(store)).Handler()

	asCommunity(t, func() {
		// Test 0: the one-click reconcile is Team, and the gate fires before the body is read.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/drift/reconcile",
			strings.NewReader(`{}`)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("reconcile = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Team license") ||
			!strings.Contains(rec.Body.String(), "switchtender.com/pricing") {
			t.Errorf("reconcile refusal does not teach the fix: %s", rec.Body.String())
		}

		// Test 1: one plain require-approval policy is Community and must stay allowed.
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
			strings.NewReader(`{"name":"hold prod"}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("the one Community policy = %d, want 201: %s", rec.Code, rec.Body.String())
		}

		// Test 2: the second policy is where Team begins.
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
			strings.NewReader(`{"name":"hold stage"}`)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("second policy = %d, want 403: %s", rec.Code, rec.Body.String())
		}

		// Test 3: a deny policy is the full engine even as the first policy.
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/policies/nope",
			strings.NewReader(`{"name":"hold prod","effect":"deny"}`)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("deny via update = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	// Test 4: with the Team license back, the same second policy saves, so the gate is the only
	// thing that refused it.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
		strings.NewReader(`{"name":"hold stage","effect":"deny"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("licensed deny policy = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// TestChangeRegisterIsTeamOverHTTP pins the period register to Team on the HTTP surface. The CLI
// gated it from the start and this endpoint did not, so a Community install served the paid
// artifact to anyone who pressed the button on the audit page. The free proofs beside it, the
// signed bundle and the chain verification, must stay open at the same time, since a gate that
// also caught those would break the promise that proofs are free on every tier.
func TestChangeRegisterIsTeamOverHTTP(t *testing.T) {
	audits := audit.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithAudit(audits)).Handler()

	asCommunity(t, func() {
		// Test 0: the register refuses, names Team, and points at the price list.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/register", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("register = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Team license") ||
			!strings.Contains(rec.Body.String(), "switchtender.com/pricing") {
			t.Errorf("register refusal does not teach the fix: %s", rec.Body.String())
		}

		// Test 1: chain verification is free on every tier.
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("verify on Community = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	// Test 2: with the license back the same request renders, so the gate is what refused it.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/register", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("licensed register = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestProGatesHoldTheLine covers what a Pro install sees: five policies fit, the sixth refuses
// toward Team, and the full engine stays refused even with a live Pro license installed.
func TestProGatesHoldTheLine(t *testing.T) {
	store := policy.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithPolicies(store)).Handler()

	prior := license.Current()
	license.Set(&license.License{Claims: license.Claims{
		Tier: license.TierPro, Org: "Acme", Expires: "2099-01-01T00:00:00Z"}})
	defer license.Set(prior)

	// Test 0: five plain policies are inside the Pro cap.
	for i := range 5 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
			strings.NewReader(fmt.Sprintf(`{"name":"hold %d"}`, i))))
		if rec.Code != http.StatusCreated {
			t.Fatalf("Pro policy %d = %d, want 201: %s", i, rec.Code, rec.Body.String())
		}
	}

	// Test 1: the sixth refuses in one line pointing at Team.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
		strings.NewReader(`{"name":"hold six"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sixth Pro policy = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Team removes the cap") {
		t.Errorf("sixth-policy refusal does not teach the fix: %s", rec.Body.String())
	}

	// Test 2: a deny policy is the full engine, which Pro does not include.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
		strings.NewReader(`{"name":"deny prod","effect":"deny"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Pro deny policy = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Pro license does not include") {
		t.Errorf("full-engine refusal does not name the Pro tier: %s", rec.Body.String())
	}
}
