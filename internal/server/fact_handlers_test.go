package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// factsFixture seeds an inventory, two runs, and cached facts for three hosts: two gathered by a
// run on the inventory the caller is granted, one gathered by a run on an inventory they are not.
func factsFixture(t *testing.T) (inventory.Store, factcache.Store, run.Store) {
	t.Helper()
	ctx := context.Background()
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: "web01\nweb02\nweb03\n", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	store := run.NewMemStore()
	for _, r := range []*run.Run{
		{ID: "run_ours", Playbook: "p.yml", InventoryID: "inv_1", Status: run.StatusSucceeded,
			CreatedAt: time.Now()},
		{ID: "run_theirs", Playbook: "p.yml", InventoryID: "inv_other", Status: run.StatusSucceeded,
			CreatedAt: time.Now()},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	facts := factcache.NewMemStore()
	doc := json.RawMessage(`{"ansible_distribution":"Debian",` +
		`"ansible_env":{"DB_PASSWORD":"s3cr3t-value"}}`)
	if err := facts.SaveFacts(ctx, []factcache.Entry{
		{InventoryID: "inv_1", Host: "web01", Facts: doc, RunID: "run_ours", ModifiedAt: time.Now()},
		{InventoryID: "inv_1", Host: "web02", Facts: doc, RunID: "run_ours", ModifiedAt: time.Now()},
		{InventoryID: "inv_1", Host: "web03", Facts: doc, RunID: "run_theirs", ModifiedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	return inventories, facts, store
}

// TestCachedFactsFollowTheInventoryAndTheRun pins who may read cached facts: read on the inventory,
// and only for hosts whose gathering run the caller may also read. Below admin the values of
// secret-looking keys are masked, because a fact document carries the remote user's environment.
func TestCachedFactsFollowTheInventoryAndTheRun(t *testing.T) {
	t.Parallel()
	inventories, facts, store := factsFixture(t)
	granted := Actor{UserID: "user_granted", Role: user.RoleOperator}
	admin := Actor{UserID: "user_admin", Role: user.RoleAdmin}
	stranger := Actor{UserID: "user_nobody", Role: user.RoleOperator}

	t.Run("list", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			WantHosts  []string
			Actor      Actor
			WantStatus int
		}{{ // Test 0: The granted operator sees the hosts its runs gathered, not the other run's.
			Actor: granted, WantStatus: http.StatusOK, WantHosts: []string{"web01", "web02"},
		}, { // Test 1: An admin sees every cached host.
			Actor: admin, WantStatus: http.StatusOK, WantHosts: []string{"web01", "web02", "web03"},
		}, { // Test 2: A caller with no grant on the inventory is refused.
			Actor: stranger, WantStatus: http.StatusForbidden,
		}}
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
				t.Parallel()
				req := withActor(t, http.MethodGet, "/v1/inventories/inv_1/facts", test.Actor)
				req.SetPathValue("id", "inv_1")
				rec := httptest.NewRecorder()
				listFactsHandler(inventories, facts, store, restrictedAuthz(t, "user_granted", "inv_1"),
					false, zap.NewNop()).ServeHTTP(rec, req)
				if rec.Code != test.WantStatus {
					t.Fatalf("status = %d, want %d (%s)", rec.Code, test.WantStatus, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "s3cr3t-value") {
					t.Errorf("the summary listing carries a fact document: %s", rec.Body.String())
				}
				var got listFactsResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				var hosts []string
				for _, e := range got.Hosts {
					hosts = append(hosts, e.Host)
				}
				if diff := cmp.Diff(test.WantHosts, hosts, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("hosts mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("one host", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			Host       string
			Actor      Actor
			WantStatus int
			WantSecret bool
		}{{ // Test 0: The granted operator reads the document with the secret masked.
			Host: "web01", Actor: granted, WantStatus: http.StatusOK,
		}, { // Test 1: An admin reads it whole.
			Host: "web01", Actor: admin, WantStatus: http.StatusOK, WantSecret: true,
		}, { // Test 2: A host whose gathering run the caller may not read is not found.
			Host: "web03", Actor: granted, WantStatus: http.StatusNotFound,
		}, { // Test 3: A host never cached is not found.
			Host: "web99", Actor: admin, WantStatus: http.StatusNotFound,
		}}
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
				t.Parallel()
				req := withActor(t, http.MethodGet, "/v1/inventories/inv_1/facts/"+test.Host, test.Actor)
				req.SetPathValue("id", "inv_1")
				req.SetPathValue("host", test.Host)
				rec := httptest.NewRecorder()
				hostCachedFactsHandler(inventories, facts, store,
					restrictedAuthz(t, "user_granted", "inv_1"), false, zap.NewNop()).ServeHTTP(rec, req)
				if rec.Code != test.WantStatus {
					t.Fatalf("status = %d, want %d (%s)", rec.Code, test.WantStatus, rec.Body.String())
				}
				if got := strings.Contains(rec.Body.String(), "s3cr3t-value"); got != test.WantSecret {
					t.Errorf("secret disclosed = %v, want %v: %s", got, test.WantSecret, rec.Body.String())
				}
				if test.WantStatus == http.StatusOK && !strings.Contains(rec.Body.String(), "Debian") {
					t.Errorf("the document lost its ordinary facts: %s", rec.Body.String())
				}
			})
		}
	})
}

// TestClearingCachedFactsTakesManage pins that clearing a host's cached facts is a change to the
// inventory, so use of it is not enough, and that a cleared host is gone.
func TestClearingCachedFactsTakesManage(t *testing.T) {
	t.Parallel()
	inventories, facts, _ := factsFixture(t)
	clearAs := func(a Actor, host string) int {
		req := withActor(t, http.MethodDelete, "/v1/inventories/inv_1/facts/"+host, a)
		req.SetPathValue("id", "inv_1")
		req.SetPathValue("host", host)
		rec := httptest.NewRecorder()
		clearHostFactsHandler(inventories, facts, restrictedAuthz(t, "user_granted", "inv_1"),
			zap.NewNop()).ServeHTTP(rec, req)
		return rec.Code
	}
	operator := Actor{UserID: "user_granted", Role: user.RoleOperator}
	if code := clearAs(operator, "web01"); code != http.StatusForbidden {
		t.Errorf("clearing with use only: status = %d, want 403", code)
	}
	admin := Actor{UserID: "user_admin", Role: user.RoleAdmin}
	if code := clearAs(admin, "web01"); code != http.StatusOK {
		t.Errorf("clearing as admin: status = %d, want 200", code)
	}
	if _, err := facts.Facts(context.Background(), "inv_1", "web01"); err == nil {
		t.Error("the cleared host still has cached facts")
	}
	if code := clearAs(admin, "web01"); code != http.StatusNotFound {
		t.Errorf("clearing again: status = %d, want 404", code)
	}
}
