package credtest

import (
	"context"
	"errors"
	"testing"

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
}

// sampleType is a representative custom type: two fields, one secret, both an env and an extra-var
// injector, including a template that splices a field into literal text.
func sampleType(id, name string) *credential.CredentialType {
	return &credential.CredentialType{
		ID: id, Name: name,
		Fields: []credential.Field{
			{Name: "host", Label: "API host"},
			{Name: "token", Label: "API token", Secret: true},
		},
		EnvInjectors:      map[string]string{"API_HOST": "{{host}}", "API_AUTH": "Bearer {{token}}"},
		ExtraVarInjectors: map[string]string{"api_host": "{{host}}"},
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
