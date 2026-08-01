package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestFleetSnapshotStaysInsideTheCallersGrants checks that the material an AI answer is drawn from
// is bounded by what the caller may read.
//
// The snapshot was assembled with an empty filter and no authorization, so a viewer holding a grant
// on one run got a snapshot naming every run in the install: playbook names, the first line of shell
// and Terraform commands, and which hosts have drifted. The model then repeats that in prose, which
// is a comfortable way for a boundary to leak, and the endpoint needs only the viewer role.
func TestFleetSnapshotStaysInsideTheCallersGrants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	mine := &run.Run{
		ID: "run_mine", Playbook: "mine.yml", InventoryID: "inv_mine",
		Status: run.StatusSucceeded, CreatedAt: time.Now(),
	}
	theirs := &run.Run{
		ID: "run_theirs", Playbook: "their-secret-project.yml", InventoryID: "inv_theirs",
		Status: run.StatusFailed, CreatedAt: time.Now().Add(-time.Minute),
	}
	for _, r := range []*run.Run{mine, theirs} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	// Strict grants, and the caller may use only their own inventory.
	authz := &authorizer{
		strict: true,
		grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"inv_mine": {{Subject: "user_1", Access: grant.AccessUse}},
		}},
	}
	actorCtx := context.WithValue(ctx, actorKey{},
		Actor{UserID: "user_1", Role: user.RoleViewer})

	got, err := buildFleetSnapshot(actorCtx, store, authz)
	if err != nil {
		t.Fatalf("buildFleetSnapshot() error = %v", err)
	}
	if strings.Contains(got, "their-secret-project.yml") || strings.Contains(got, "run_theirs") {
		t.Errorf("the snapshot names a run the caller may not read, so the model can repeat it "+
			"back to them:\n%s", got)
	}
	if !strings.Contains(got, "run_mine") {
		t.Errorf("the snapshot omits the caller's own run, so the answer has nothing to work "+
			"from:\n%s", got)
	}
}
