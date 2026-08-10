package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/template"
)

// TestSeedExamples checks the starter templates seed once, are all runnable standalone, and that a
// second seed adds nothing.
func TestSeedExamples(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	// Every starter template must run with no project, inventory, or credential, or the whole point
	// of seeding a fresh install is lost. That means the Bash tool and a non-empty command, and no
	// reference to anything the user has not set up.
	names := map[string]bool{}
	for _, tmpl := range starterTemplates(now) {
		if tmpl.Tool != "bash" {
			t.Errorf("starter %q uses tool %q, want bash so it runs standalone", tmpl.Name, tmpl.Tool)
		}
		if tmpl.Command == "" {
			t.Errorf("starter %q has an empty command", tmpl.Name)
		}
		if tmpl.ProjectID != "" || tmpl.InventoryID != "" || len(tmpl.CredentialIDs) != 0 {
			t.Errorf("starter %q depends on a project, inventory, or credential", tmpl.Name)
		}
		if names[tmpl.Name] {
			t.Errorf("duplicate starter name %q", tmpl.Name)
		}
		names[tmpl.Name] = true
	}

	store := template.NewMemStore()
	added, err := seedExamples(ctx, store, now)
	if err != nil {
		t.Fatalf("seedExamples() error = %v", err)
	}
	if added != len(names) {
		t.Errorf("first seed added %d, want %d", added, len(names))
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != len(names) {
		t.Errorf("store holds %d templates, want %d", len(list), len(names))
	}

	// Seeding again must be a no-op, so running the command twice never doubles the list.
	again, err := seedExamples(ctx, store, now)
	if err != nil {
		t.Fatalf("second seedExamples() error = %v", err)
	}
	if again != 0 {
		t.Errorf("second seed added %d, want 0", again)
	}
}
