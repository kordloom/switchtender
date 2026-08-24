package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestHeadIsNotRefusedOnPublicRoutes covers a deployment footgun that turned on the moment an
// operator created their first API token.
//
// Go's ServeMux answers a GET pattern for HEAD automatically, but the gate's public carve-outs
// matched GET alone, so the gate refused every HEAD before the mux saw it. An uptime monitor or load
// balancer configured for HEAD /healthz, which is a very common default, reported the install
// permanently down, and a third party that HEADs the trust document before fetching it could not
// read the signing key at all.
func TestHeadIsNotRefusedOnPublicRoutes(t *testing.T) {
	t.Parallel()
	gate := &authGate{log: zap.NewNop(), authz: &authorizer{}}

	for _, path := range []string{
		"/healthz", "/readyz", "/ui/", "/.well-known/loomseal.json",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req := httptest.NewRequest(method, path, nil)
			if gate.protects(req) {
				t.Errorf("%s %s is gated, so a monitor or a relying party is refused", method, path)
			}
		}
	}

	// A mutation is still gated however it is spelled, so this did not open anything.
	if !gate.protects(httptest.NewRequest(http.MethodPost, "/v1/runs", nil)) {
		t.Error("POST /v1/runs is no longer gated")
	}
	if !gate.protects(httptest.NewRequest(http.MethodHead, "/v1/runs", nil)) {
		t.Error("HEAD /v1/runs is no longer gated, so a protected route leaked through the carve-out")
	}
}

// TestMetricsWithheldFromACallerWhoMayReadNothing covers the one endpoint that skipped the grant
// filter its API equivalents apply.
//
// GET /v1/workers and GET /v1/fleet nil their lists for a caller who may read no runs. /metrics took
// no authorizer at all, so under strict grants a viewer in one tenant could scrape every executor
// name, every queue name, the estate's host count, how many hosts are failing, and the audit-chain
// gauges for work they are refused by name everywhere else.
func TestMetricsWithheldFromACallerWhoMayReadNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded,
		ProjectID: "proj_secret", Queue: "prod", ClaimedBy: "worker-prod",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Strict grants, and this caller holds none.
	authz := &authorizer{strict: true, grants: &fakeGrants{}}
	handler := metricsHandler(store, nil, authz, zap.NewNop())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil).WithContext(
		context.WithValue(ctx, actorKey{}, Actor{UserID: "user_1", Role: user.RoleViewer}))
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an empty body", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("a caller who may read nothing scraped the estate:\n%s", body)
	}

	// An unrestricted caller still gets the series, or the endpoint is simply gone.
	open := &authorizer{grants: &fakeGrants{}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil).WithContext(
		context.WithValue(ctx, actorKey{}, Actor{UserID: "user_2", Role: user.RoleAdmin}))
	metricsHandler(store, nil, open, zap.NewNop())(rec, req)
	if rec.Body.Len() == 0 {
		t.Error("an unrestricted caller got no metrics at all")
	}
}

// TestCrossSiteWritesAreRefused covers the drive-by an install in open mode was reachable by.
//
// Nothing looked at Origin or Sec-Fetch-Site, and the JSON decoder accepts any body whatever the
// content type claims. A fresh install on a loopback bind runs open by design, so a cross-site page
// could POST a run as a CORS simple request, with Content-Type text/plain and mode no-cors so it
// never needs to read the response. An operator browsing anywhere while their own install was up had
// a run executed against their fleet.
func TestCrossSiteWritesAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Method    string
		Path      string
		FetchSite string
		Origin    string
		WantBlock bool
	}{{ // Test 0: The attack. A page on another site posting a run.
		Name: "cross-site post", Method: http.MethodPost, Path: "/v1/runs",
		FetchSite: "cross-site", WantBlock: true,
	}, { // Test 1: An older browser that sends Origin but not Sec-Fetch-Site.
		Name: "foreign origin", Method: http.MethodPost, Path: "/v1/runs",
		Origin: "https://evil.example", WantBlock: true,
	}, { // Test 2: A sibling subdomain is still not this origin.
		Name: "same-site post", Method: http.MethodPost, Path: "/v1/runs",
		FetchSite: "same-site", WantBlock: true,
	}, { // Test 3: The product's own UI, which must keep working.
		Name: "same-origin post", Method: http.MethodPost, Path: "/v1/runs",
		FetchSite: "same-origin",
	}, { // Test 4: curl and the CLI send neither header.
		Name: "no browser headers", Method: http.MethodPost, Path: "/v1/runs",
	}, { // Test 5: A read is not a state change, so a cross-site GET is not this control's business.
		Name: "cross-site get", Method: http.MethodGet, Path: "/v1/runs", FetchSite: "cross-site",
	}, { // Test 6: The SAML assertion arrives as a cross-site POST by design, through the operator's
		// browser, and has its own InResponseTo defense.
		Name: "saml acs", Method: http.MethodPost, Path: "/auth/saml/acs", FetchSite: "cross-site",
	}, { // Test 7: A forge delivering a webhook is server-side and sends neither header.
		Name: "webhook delivery", Method: http.MethodPost, Path: "/hooks/tok",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(test.Method, test.Path, nil)
			req.Host = "switchtender.example"
			if test.FetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", test.FetchSite)
			}
			if test.Origin != "" {
				req.Header.Set("Origin", test.Origin)
			}
			if got := crossSiteWrite(req); got != test.WantBlock {
				t.Errorf("%s: blocked = %v, want %v", test.Name, got, test.WantBlock)
			}
		})
	}

	// An Origin naming this very host is the UI itself and must pass.
	same := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
	same.Host = "switchtender.example"
	same.Header.Set("Origin", "https://switchtender.example")
	if crossSiteWrite(same) {
		t.Error("the product's own origin was refused, so the UI cannot submit anything")
	}
}
