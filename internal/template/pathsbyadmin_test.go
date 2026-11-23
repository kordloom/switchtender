package template

import (
	"context"
	"testing"
	"time"
)

// testTemplatePathsByAdmin proves a template's PathsByAdmin survives Save and Get, and survives an
// Update that flips it from true to false.
func testTemplatePathsByAdmin(t *testing.T, store Store) {
	ctx := context.Background()
	tests := []struct {
		// ID is the template identifier.
		ID string
		// Saved is the fact on Save.
		Saved bool
	}{{ // Test 0: A template an admin saved.
		ID: "tpl_by_admin", Saved: true,
	}, { // Test 1: A template anyone else saved.
		ID: "tpl_by_other", Saved: false,
	}}
	for testNum, test := range tests {
		tpl := &Template{ID: test.ID, Name: "deploy", Playbook: "site.yml",
			PathsByAdmin: test.Saved, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
		if err := store.Save(ctx, tpl); err != nil {
			t.Fatalf("test %d: Save() error = %v", testNum, err)
		}
		got, err := store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		if got.PathsByAdmin != test.Saved {
			t.Errorf("test %d: after Save PathsByAdmin = %t, want %t", testNum, got.PathsByAdmin, test.Saved)
		}
		tpl.PathsByAdmin = !test.Saved
		if err := store.Update(ctx, tpl); err != nil {
			t.Fatalf("test %d: Update() error = %v", testNum, err)
		}
		got, err = store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("test %d: Get() after Update error = %v", testNum, err)
		}
		if got.PathsByAdmin != !test.Saved {
			t.Errorf("test %d: after Update PathsByAdmin = %t, want %t", testNum, got.PathsByAdmin,
				!test.Saved)
		}
	}
}

// TestMemStoreKeepsTemplatePathsByAdmin runs the round trip on the memory store.
func TestMemStoreKeepsTemplatePathsByAdmin(t *testing.T) {
	t.Parallel()
	testTemplatePathsByAdmin(t, NewMemStore())
}
