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

// TestTheEstateWhenTheInventoryIsDeleted asks what happens to history when the configuration that
// produced it is removed.
//
// A run is scoped by the objects it names, and facts are scoped by their run. Delete the inventory
// and the grant on it goes too, so a grant-restricted caller loses every reading gathered through
// it: the same shape as a purged run, which is why this is worth knowing rather than assuming.
//
// This records the behavior rather than asserting a preference, because both answers are defensible
// and the one that is wrong is not knowing which one ships.
func TestTheEstateWhenTheInventoryIsDeleted(t *testing.T) {
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
	if err := store.Save(ctx, &run.Run{ID: "run_1", Playbook: "site.yml",
		Status: run.StatusSucceeded, CreatedAt: at, InventoryID: "inv_1"}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := store.SaveHostFacts(ctx, "run_1", []run.HostFacts{{
		Host: "web01", Facts: map[string]string{"kernel": "6.1.0"}, GatheredAt: at,
	}}); err != nil {
		t.Fatalf("save facts: %v", err)
	}

	estateFor := func(grants *fakeGrants) int {
		t.Helper()
		handler := estateHandler(store, &authorizer{strict: true, grants: grants}, zap.NewNop())
		req := httptest.NewRequest(http.MethodGet, "/v1/estate", nil).WithContext(
			actorCtx(Actor{UserID: "u1", Role: user.RoleViewer}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var got struct {
			Hosts []run.HostFacts `json:"hosts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(got.Hosts)
	}

	// While the inventory exists and is granted, the reading is readable.
	granted := &fakeGrants{byObject: map[string][]*grant.Grant{
		"inv_1": {{Subject: "u1", Access: grant.AccessUse}},
	}}
	if n := estateFor(granted); n != 1 {
		t.Fatalf("estate holds %d hosts while the inventory is granted, want 1", n)
	}

	// Deleting the inventory takes its grants with it, which is what removing an object means.
	deleted := &fakeGrants{byObject: map[string][]*grant.Grant{}}
	n := estateFor(deleted)
	t.Logf("after the inventory is deleted, a grant-restricted caller sees %d host(s)", n)
	if n != 0 {
		t.Errorf("estate holds %d hosts after the inventory was deleted. A run is scoped by the "+
			"objects it names, so this reads as the scoping being skipped rather than as history "+
			"outliving configuration", n)
	}
}
