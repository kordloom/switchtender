package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheEstateSaysWhatItWithheldWhenTheRunIsGone covers the exact collision this feature has with
// retention, with both halves real rather than reasoned about.
//
// Facts outlive the runs that gathered them, on purpose, and the estate's whole point is old dates.
// So the two meet by definition: ask far enough back and the governing run has been deleted. Access
// is decided by resolving that run, and a deleted run resolves to nothing, so the row is withheld.
//
// Withholding is right. Silence is not. Without the count a grant-restricted caller asking about
// last year is handed an empty estate and told nothing, and the obvious reading is that the fleet
// did not exist.
func TestTheEstateSaysWhatItWithheldWhenTheRunIsGone(t *testing.T) {
	t.Parallel()
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	ctx := context.Background()
	store := run.NewMemStore()
	old := time.Now().Add(-90 * 24 * time.Hour)

	// A readable run the caller is granted, and one it is not. Both gather a host.
	readable := &run.Run{ID: "run_granted", Playbook: "p", Status: run.StatusSucceeded,
		ProjectID: "proj_granted", CreatedAt: old}
	hidden := &run.Run{ID: "run_other", Playbook: "p", Status: run.StatusSucceeded,
		ProjectID: "proj_other", CreatedAt: old}
	for _, r := range []*run.Run{readable, hidden} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}
	for host, r := range map[string]*run.Run{"web01": readable, "db01": hidden} {
		if err := store.SaveHostFacts(ctx, r.ID, []run.HostFacts{{
			Host: host, Facts: map[string]string{"kernel": "6.1.0"}, GatheredAt: old,
		}}); err != nil {
			t.Fatalf("save facts for %s: %v", host, err)
		}
	}

	// Retention deletes both runs, the way --retain-runs would after ninety days.
	if _, err := store.PurgeRunsBefore(ctx, time.Now()); err != nil {
		t.Fatalf("purge: %v", err)
	}

	authz := &authorizer{strict: true, grants: &fakeGrants{byObject: map[string][]*grant.Grant{
		"proj_granted": {{Subject: "u1", Access: grant.AccessUse}},
	}}}
	handler := estateHandler(store, authz, zap.NewNop())
	req := httptest.NewRequest(http.MethodGet, "/v1/estate", nil).WithContext(
		actorCtx(Actor{UserID: "u1", Role: user.RoleViewer}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Hosts    []run.HostFacts `json:"hosts"`
		Total    int             `json:"total"`
		Withheld int             `json:"withheld"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Both readings survived the purge, and neither is readable now: resolving a deleted run to
	// decide access can only fail. So the estate is empty, and the count is the only thing that
	// says why.
	if got.Withheld == 0 {
		t.Errorf("the estate withheld nothing and returned %d hosts. Either the readings did not "+
			"survive the purge, or rows were dropped with nothing to say so: %+v", got.Total, got)
	}
	if got.Total != len(got.Hosts) {
		t.Errorf("total %d does not match the %d hosts shown", got.Total, len(got.Hosts))
	}
	// And a withheld row is genuinely absent rather than leaked.
	for _, h := range got.Hosts {
		if h.Host == "db01" {
			t.Error("a host gathered by an ungranted run was returned")
		}
	}
}
