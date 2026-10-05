package credtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
)

// TypeContract runs the credential.TypeStore contract against a fresh store from newStore, so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart on how a custom type round-trips.
func TypeContract(t *testing.T, newStore func() credential.TypeStore) {
	t.Helper()
	t.Run("round trip preserves fields and injectors", func(t *testing.T) {
		testTypeRoundTrip(t, newStore())
	})
	t.Run("list is ordered and delete removes", func(t *testing.T) {
		testTypeListAndDelete(t, newStore())
	})
	t.Run("missing type reports not found", func(t *testing.T) {
		testTypeMissing(t, newStore())
	})
	t.Run("list is oldest first on every backend", func(t *testing.T) {
		testTypeListOrder(t, newStore())
	})
	t.Run("an imported type keeps its origin", func(t *testing.T) {
		testTypeOrigin(t, newStore())
	})
}

// testTypeOrigin pins that a type's origin survives storage, on save and on overwrite, because it
// is what lets an AWX type keep a file no injector references. A backend that dropped it would turn
// every stored imported type with such a file into one that no longer validates, and a run of it
// would stop warning about the file.
func testTypeOrigin(t *testing.T, store credential.TypeStore) {
	ctx := context.Background()
	imported := sampleType("ctype_awx", "Legacy cert")
	imported.Origin = credential.OriginAWX
	// The single file is written and nothing hands its path over, which an imported type may do.
	imported.FileInjectors = map[string]string{"template": "{{ ca }}"}
	imported.EnvInjectors = map[string]string{"API_HOST": "{{host}}"}
	if err := store.Save(ctx, imported); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "ctype_awx")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(imported, got); diff != "" {
		t.Errorf("round trip changed the imported type (-want +got):\n%s", diff)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a stored imported type no longer validates: %v", err)
	}
	if diff := cmp.Diff([]string{credential.SingleFile}, got.UnreferencedFiles()); diff != "" {
		t.Errorf("unreferenced files of the stored type (-want +got):\n%s", diff)
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var origins []string
	for _, l := range listed {
		origins = append(origins, l.Origin)
	}
	if diff := cmp.Diff([]string{credential.OriginAWX}, origins); diff != "" {
		t.Errorf("origins List() returned (-want +got):\n%s", diff)
	}
	// An overwrite carries the origin it is given, so a save that clears it clears it.
	cleared := *got
	cleared.Origin = ""
	if err := store.Save(ctx, &cleared); err != nil {
		t.Fatalf("Save() overwrite error = %v", err)
	}
	if again, err := store.Get(ctx, "ctype_awx"); err != nil || again.Origin != "" {
		t.Errorf("Get() after an overwrite clearing the origin = %+v, %v, want no origin", again, err)
	}
}

// testTypeListOrder pins that List returns types oldest first identically on every backend.
//
// The ids are random, so a backend that ordered by id returned effectively random order while the
// in-memory one returned insertion order, and the "oldest first" the interface documents held on
// neither. The types here are created in an order that disagrees with their id sort, so a backend
// ordering by id fails this and a backend ordering by creation time passes.
func testTypeListOrder(t *testing.T, store credential.TypeStore) {
	ctx := context.Background()
	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	// Created third-to-first in time, with ids that sort the opposite way.
	created := []struct {
		id string
		at time.Time
	}{
		{"ctype_zzz", base},
		{"ctype_mmm", base.Add(time.Minute)},
		{"ctype_aaa", base.Add(2 * time.Minute)},
	}
	for _, c := range created {
		typ := sampleType(c.id, c.id)
		typ.CreatedAt = c.at
		if err := store.Save(ctx, typ); err != nil {
			t.Fatalf("Save(%s) error = %v", c.id, err)
		}
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []string{"ctype_zzz", "ctype_mmm", "ctype_aaa"}
	if len(got) != len(want) {
		t.Fatalf("List() returned %d types, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			var order []string
			for _, g := range got {
				order = append(order, g.ID)
			}
			t.Fatalf("List() order = %v, want oldest first %v: a backend ordering by id rather "+
				"than creation time gives a different answer here", order, want)
		}
	}
}

// sampleType is a representative custom type: three fields, one secret and one multiline, an env,
// an extra-var, and a file injector, including a template that splices a field into literal text and
// one that hands the written file's path over.
func sampleType(id, name string) *credential.CredentialType {
	return &credential.CredentialType{
		ID: id, Name: name, CreatedAt: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		Fields: []credential.Field{
			{Name: "host", Label: "API host"},
			{Name: "token", Label: "API token", Secret: true},
			{Name: "ca", Label: "CA bundle", Multiline: true},
		},
		EnvInjectors: map[string]string{
			"API_HOST": "{{host}}", "API_AUTH": "Bearer {{token}}", "API_CA": "{{tower.filename.ca}}",
		},
		ExtraVarInjectors: map[string]string{"api_host": "{{host}}"},
		FileInjectors:     map[string]string{"template.ca": "# bundle\n{{ ca }}\n"},
	}
}

func testTypeRoundTrip(t *testing.T, store credential.TypeStore) {
	ctx := context.Background()
	want := sampleType("ctype_1", "Datadog")
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "ctype_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip changed the type (-want +got):\n%s", diff)
	}
	// A stored type still validates, since an injector reference that survived storage must still
	// resolve.
	if err := got.Validate(); err != nil {
		t.Errorf("a stored type no longer validates: %v", err)
	}
	// Overwriting by id replaces rather than duplicates.
	updated := sampleType("ctype_1", "Datadog v2")
	if err := store.Save(ctx, updated); err != nil {
		t.Fatalf("Save() update error = %v", err)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 1 || all[0].Name != "Datadog v2" {
		t.Errorf("List() = %d types, want the single replaced one", len(all))
	}
}

func testTypeListAndDelete(t *testing.T, store credential.TypeStore) {
	ctx := context.Background()
	for _, id := range []string{"ctype_a", "ctype_b", "ctype_c"} {
		if err := store.Save(ctx, sampleType(id, id)); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List() = %d, want 3", len(all))
	}
	if err := store.Delete(ctx, "ctype_b"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "ctype_b"); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Get() after delete error = %v, want ErrNotFound", err)
	}
	if remaining, err := store.List(ctx); err != nil || len(remaining) != 2 {
		t.Errorf("List() after delete = (%d, %v), want 2", len(remaining), err)
	}
}

func testTypeMissing(t *testing.T, store credential.TypeStore) {
	ctx := context.Background()
	if _, err := store.Get(ctx, "ctype_nope"); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "ctype_nope"); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Delete(missing) error = %v, want ErrNotFound", err)
	}
}
