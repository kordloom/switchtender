package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheEstateStaysInsideTheTenant covers the estate against organizations rather than grants.
//
// Grants and organizations are two ways a caller is scoped, and the estate was built against the
// first and never tried against the second. That gap has produced a real leak here before: the
// aggregate views reported totals across every tenant until they were fixed to compute inside one.
//
// The estate is the widest read in the product, one row per host in the fleet, so it is the worst
// place for the same mistake.
func TestTheEstateStaysInsideTheTenant(t *testing.T) {
	t.Parallel()
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	ctx := context.Background()
	store := run.NewMemStore()
	at := time.Now().Add(-time.Hour)

	// One run per tenant, each scoped by a project that tenant owns, each gathering its own host.
	for _, tc := range []struct{ runID, project, host string }{
		{"run_a", "proj_a", "web-a"},
		{"run_b", "proj_b", "web-b"},
	} {
		if err := store.Save(ctx, &run.Run{ID: tc.runID, Playbook: "site.yml",
			Status: run.StatusSucceeded, CreatedAt: at, ProjectID: tc.project}); err != nil {
			t.Fatalf("save %s: %v", tc.runID, err)
		}
		if err := store.SaveHostFacts(ctx, tc.runID, []run.HostFacts{{
			Host: tc.host, Facts: map[string]string{"kernel": "6.1.0"}, GatheredAt: at,
		}}); err != nil {
			t.Fatalf("save facts for %s: %v", tc.host, err)
		}
	}

	authzFor := orgOwnedFixture(t)
	handler := estateHandler(store, authzFor(true), zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/v1/estate", nil).WithContext(
		actorCtx(Actor{UserID: "user_member_a", Role: user.RoleViewer}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Hosts []run.HostFacts `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, h := range got.Hosts {
		if h.Host == "web-b" {
			t.Error("a member of org_a sees a host gathered by a run scoped to org_b. The estate " +
				"is the widest read in the product, so a tenant leak here exposes the whole fleet")
		}
	}
	// And the tenant's own host is still visible, or the isolation is just a broken endpoint.
	var sawOwn bool
	for _, h := range got.Hosts {
		if h.Host == "web-a" {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Errorf("a member of org_a cannot see a host gathered by their own org's run: %+v", got.Hosts)
	}
}

// TestAChangeStaysInsideTheTenant covers the change view against organizations.
//
// It filters its members with its own loop rather than sharing the estate's, so passing there says
// nothing about here. A change is a view over runs, so showing a member run the caller could not
// have listed directly would be a way to read another tenant's history by guessing a label.
func TestAChangeStaysInsideTheTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	at := time.Now().Add(-time.Hour)

	// Both tenants happen to use the same change name, which is the case that matters: a label is
	// not a secret and two orgs can pick the same word.
	const change = "OPS-482"
	for _, tc := range []struct{ runID, project string }{
		{"run_a", "proj_a"},
		{"run_b", "proj_b"},
	} {
		if err := store.Save(ctx, &run.Run{ID: tc.runID, Playbook: "site.yml",
			Status: run.StatusSucceeded, CreatedAt: at, ProjectID: tc.project,
			Labels: map[string]string{run.ChangeLabel: change}}); err != nil {
			t.Fatalf("save %s: %v", tc.runID, err)
		}
	}

	authzFor := orgOwnedFixture(t)
	handler := changeHandler(store, authzFor(true), zap.NewNop())
	req := httptest.NewRequest(http.MethodGet, "/v1/changes/"+change, nil).WithContext(
		actorCtx(Actor{UserID: "user_member_a", Role: user.RoleViewer}))
	req.SetPathValue("change", change)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Runs     []*run.Run `json:"runs"`
		Total    int        `json:"total"`
		Withheld int        `json:"withheld"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 {
		t.Fatalf("the change holds %d runs for a member of org_a, want only their own: %+v",
			got.Total, got.Runs)
	}
	if got.Runs[0].ID != "run_a" {
		t.Errorf("the change shows %s, want run_a. Another tenant's run is readable by guessing a "+
			"label, which is not a secret", got.Runs[0].ID)
	}
	if got.Withheld != 1 {
		t.Errorf("withheld = %d, want 1: the other tenant's member is either leaking or vanishing "+
			"without a count", got.Withheld)
	}
}
