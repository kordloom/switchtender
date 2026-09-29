package server

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// actorCtx puts an authenticated actor on a background context, the way the auth middleware does.
func actorCtx(a Actor) context.Context {
	return context.WithValue(context.Background(), actorKey{}, a)
}

// TestDerivedReadFilterConsultsAgedRuns proves a grant-restricted caller still sees a derived row
// (drift, host history, host facts) whose governing run has aged past the recent-runs window. The
// filter used to build its allow-set from only the newest derivedReadScan top-level runs, so on a busy
// install every derived row governed by an older run silently vanished for the exact governed-tier
// callers the feature is for. The predicate now consults the actual governing run on demand.
func TestDerivedReadFilterConsultsAgedRuns(t *testing.T) {
	t.Parallel()
	orig := derivedReadScan
	derivedReadScan = 2
	t.Cleanup(func() { derivedReadScan = orig })

	authz := &authorizer{strict: true, grants: &fakeGrants{byObject: map[string][]*grant.Grant{
		"proj_granted": {{Subject: "u1", Access: grant.AccessUse}},
	}}}

	store := run.NewMemStore()
	base := time.Now().Add(-time.Hour)
	// The readable run is the oldest; three newer, ungranted runs push it out of the 2-run window.
	old := &run.Run{ID: "run_old", Playbook: "p", Status: run.StatusSucceeded,
		ProjectID: "proj_granted", CreatedAt: base}
	if err := store.Save(context.Background(), old); err != nil {
		t.Fatalf("save old: %v", err)
	}
	for i := 0; i < 3; i++ {
		r := &run.Run{ID: "run_new_" + string(rune('a'+i)), Playbook: "p", Status: run.StatusSucceeded,
			ProjectID: "proj_other", CreatedAt: base.Add(time.Duration(i+1) * time.Minute)}
		if err := store.Save(context.Background(), r); err != nil {
			t.Fatalf("save new %d: %v", i, err)
		}
	}

	ctx := actorCtx(Actor{UserID: "u1", Role: user.RoleViewer})
	keep, _, err := derivedReadFilter(ctx, authz, store)
	if err != nil {
		t.Fatalf("derivedReadFilter: %v", err)
	}
	if !keep("run_old") {
		t.Error("a derived row governed by an aged-out but readable run was hidden from its grantee")
	}
	if keep("run_new_a") {
		t.Error("a derived row governed by an ungranted run was shown")
	}
}
