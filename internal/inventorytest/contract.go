// Package inventorytest provides a shared behavior contract for inventory.Store implementations
// so the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package inventorytest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
)

// Contract runs the inventory.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() inventory.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("update", func(t *testing.T) { testUpdate(t, newStore()) })
	t.Run("composition round trip", func(t *testing.T) { testComposition(t, newStore()) })
}

// testComposition verifies a smart and a constructed inventory keep their kind and definition
// through save, update, get, and list, and that a static inventory reads back as static.
//
// A store that dropped the kind would turn a smart inventory into an empty static one, and every
// run against it would target nothing while reporting that it ran.
func testComposition(t *testing.T, store inventory.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	smart := &inventory.Inventory{
		ID: "inv_smart", Name: "web everywhere", Kind: inventory.KindSmart,
		HostFilter: `groups__name=web and not name__startswith="canary"`, CreatedAt: created,
	}
	built := &inventory.Inventory{
		ID: "inv_built", Name: "shut down", Kind: inventory.KindConstructed,
		InputIDs:   []string{"inv_a", "inv_b"},
		SourceVars: "groups:\n  off: state == \"shutdown\"\n", Limit: "off",
		CreatedAt: created.Add(time.Hour),
	}
	for _, i := range []*inventory.Inventory{smart, built} {
		if err := store.Save(ctx, i); err != nil {
			t.Fatalf("Save(%s) error = %v", i.ID, err)
		}
	}
	for _, want := range []*inventory.Inventory{smart, built} {
		got, err := store.Get(ctx, want.ID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", want.ID, err)
		}
		if got.Kind != want.Kind || got.HostFilter != want.HostFilter ||
			!slices.Equal(got.InputIDs, want.InputIDs) || got.SourceVars != want.SourceVars ||
			got.Limit != want.Limit {
			t.Errorf("Get(%s) = %+v, want the saved composition %+v", want.ID, got, want)
		}
	}

	if err := store.Update(ctx, &inventory.Inventory{
		ID: "inv_built", Name: "shut down", Kind: inventory.KindConstructed,
		InputIDs: []string{"inv_c"}, SourceVars: "strict: true\n", Limit: "all",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "inv_built")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !slices.Equal(got.InputIDs, []string{"inv_c"}) || got.SourceVars != "strict: true\n" ||
		got.Limit != "all" {
		t.Errorf("after update = %+v, want inputs [inv_c], strict options, limit all", got)
	}

	if err := store.Update(ctx, &inventory.Inventory{ID: "inv_smart", Name: "now static",
		Content: "[web]\nweb01\n"}); err != nil {
		t.Fatalf("Update() to static error = %v", err)
	}
	got, err = store.Get(ctx, "inv_smart")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Kind != inventory.KindStatic || got.HostFilter != "" || got.Composed() {
		t.Errorf("after update to static = %+v, want no kind and no filter", got)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[1].Kind != inventory.KindConstructed ||
		!slices.Equal(list[1].InputIDs, []string{"inv_c"}) {
		t.Errorf("List() = %+v, want the constructed inventory second with its inputs", list)
	}
}

// testUpdate verifies an update changes name and content, preserves the creation time, and reports
// ErrNotFound for an unknown id.
func testUpdate(t *testing.T, store inventory.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "old", Content: "a", CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Update(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "new", Content: "b", CredentialIDs: []string{"cred_x"},
		ContentSource: "command", ContentConfig: "sealed-cmd", Queue: "night-shift", OrgID: "org_new",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "inv_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "new" || got.Content != "b" {
		t.Errorf("Get() = %+v, want updated name and content", got)
	}
	if got.OrgID != "org_new" {
		t.Errorf("OrgID after update = %q, want org_new", got.OrgID)
	}
	if !slices.Equal(got.CredentialIDs, []string{"cred_x"}) {
		t.Errorf("CredentialIDs after update = %v, want [cred_x]", got.CredentialIDs)
	}
	if got.ContentSource != "command" || got.ContentConfig != "sealed-cmd" {
		t.Errorf("content source after update = %q/%q, want command/sealed-cmd", got.ContentSource, got.ContentConfig)
	}
	if got.Queue != "night-shift" {
		t.Errorf("Queue after update = %q, want night-shift", got.Queue)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, created)
	}

	if err := store.Update(ctx, &inventory.Inventory{ID: "ghost", Name: "x", Content: "y"}); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Update(ghost) error = %v, want ErrNotFound", err)
	}
}

// testLifecycle verifies an inventory round trips with its content and deletes.
func testLifecycle(t *testing.T, store inventory.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	i := &inventory.Inventory{
		ID: "inv_1", Name: "fleet", Content: "[web]\nweb01\n",
		CredentialIDs: []string{"cred_a", "cred_b"},
		ContentSource: "vault", ContentConfig: "sealed-config-blob", Queue: "dmz",
		OrgID: "org_owner", CreatedAt: created,
	}
	if err := store.Save(ctx, i); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "inv_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "fleet" || got.Content != "[web]\nweb01\n" || !got.CreatedAt.Equal(created) {
		t.Errorf("Get() = %+v, want the saved inventory", got)
	}
	if got.OrgID != "org_owner" {
		t.Errorf("OrgID = %q, want org_owner", got.OrgID)
	}
	if !slices.Equal(got.CredentialIDs, []string{"cred_a", "cred_b"}) {
		t.Errorf("CredentialIDs = %v, want [cred_a cred_b]", got.CredentialIDs)
	}
	if got.ContentSource != "vault" || got.ContentConfig != "sealed-config-blob" {
		t.Errorf("content source = %q/%q, want vault/sealed-config-blob", got.ContentSource, got.ContentConfig)
	}
	if got.Queue != "dmz" {
		t.Errorf("Queue = %q, want dmz", got.Queue)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "inv_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "inv_1"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies inventories come back oldest first.
func testList(t *testing.T, store inventory.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"inv_b", "inv_a"} {
		if err := store.Save(ctx, &inventory.Inventory{
			ID: id, Name: id, Content: "x", CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "inv_a" || list[1].ID != "inv_b" {
		t.Errorf("List() order = %+v, want inv_a then inv_b", list)
	}
}
