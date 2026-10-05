package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/importer"
)

// importOrgsExport is an AWX export carrying the organization its smart inventory belongs to.
const importOrgsExport = `{
  "organizations": [{"name": "Ops"}],
  "inventory": [
    {"name": "web fleet", "organization": {"name": "Ops"}, "hosts": [{"name": "web1"}]},
    {"name": "all web", "kind": "smart", "organization": {"name": "Ops"},
     "host_filter": "name__startswith=web"}
  ]
}`

// TestImportPlacesSmartInventoriesInTheirOrganization pins the command line's side of placing an
// imported smart inventory: the plan says where each inventory lands, and applying it writes the
// organization and stores both inventories in it.
func TestImportPlacesSmartInventoriesInTheirOrganization(t *testing.T) {
	// Not parallel: the import commands read package-level flag variables.
	dir := t.TempDir()
	export := filepath.Join(dir, "export.json")
	if err := os.WriteFile(export, []byte(importOrgsExport), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	db := filepath.Join(dir, "switchtender.db")
	setString(t, &importDB, db)
	setBool(t, &importApply, true)

	var stdout, stderr bytes.Buffer
	c := testCommand()
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	if err := runImport(c, export, importer.FromAWX); err != nil {
		t.Fatalf("runImport() error = %v\nstdout:\n%s", err, stdout.String())
	}
	for _, want := range []string{"Organizations: 1", "    - Ops\n",
		"    - all web (organization Ops)\n", "    - web fleet (organization Ops)\n"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the plan lacks %q:\n%s", want, stdout.String())
		}
	}

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	ctx := context.Background()
	orgs, err := bundle.Orgs().List(ctx)
	if err != nil || len(orgs) != 1 || orgs[0].Name != "Ops" {
		t.Fatalf("stored organizations = %+v, %v, want one named Ops", orgs, err)
	}
	invs, err := bundle.Inventories().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var placed []string
	for _, inv := range invs {
		if inv.OrgID == orgs[0].ID {
			placed = append(placed, inv.Name)
		}
	}
	sort.Strings(placed)
	if diff := cmp.Diff([]string{"all web", "web fleet"}, placed); diff != "" {
		t.Errorf("inventories stored in the organization (-want +got):\n%s", diff)
	}
}
