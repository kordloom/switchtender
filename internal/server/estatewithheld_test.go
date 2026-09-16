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

// TestTheEstateStillAnswersAfterItsRunsArePurged covers the exact collision this feature has with
// retention, with both halves real rather than reasoned about.
//
// Facts outlive the runs that gathered them, on purpose, and the estate's whole point is old dates.
// So the two meet by definition: ask far enough back and the governing run has been deleted.
//
// Readability used to be decided by resolving that run, and a deleted run resolves to nothing, so
// every reading it governed was withheld: a grant-restricted caller asking about last year got an
// empty estate, and the obvious reading of that is that the fleet did not exist. The purge now
// retains what decided readability, so the answer survives the run.
//
// The other half matters as much: retaining the decision must not widen what anybody can read.
func TestTheEstateStillAnswersAfterItsRunsArePurged(t *testing.T) {
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

	// Both readings survived the purge, and the granted one is still readable, because the purge
	// retained what decided readability rather than only the run. This assertion read the other way
	// until it did: the estate came back empty, and the count was the only thing saying why.
	if len(got.Hosts) != 1 {
		t.Fatalf("estate holds %d hosts after the purge, want the one the caller is granted. "+
			"Readability is being decided by resolving a run that no longer exists: %+v",
			len(got.Hosts), got)
	}
	if got.Hosts[0].Host != "web01" {
		t.Errorf("estate shows %s, want web01, the host gathered by the granted run",
			got.Hosts[0].Host)
	}
	if got.Total != len(got.Hosts) {
		t.Errorf("total %d does not match the %d hosts shown", got.Total, len(got.Hosts))
	}
	// The ungranted one is still withheld, and still counted. Retaining the decision must not have
	// widened what anybody can read: it restores the answer the caller always should have got.
	if got.Withheld != 1 {
		t.Errorf("withheld = %d, want 1. The reading gathered by a run this caller was never "+
			"granted is either leaking or vanishing without a count", got.Withheld)
	}
	for _, h := range got.Hosts {
		if h.Host == "db01" {
			t.Error("a host gathered by an ungranted run was returned: retaining the authorization " +
				"decision widened what a caller can read, which is the one thing it must not do")
		}
	}
}
