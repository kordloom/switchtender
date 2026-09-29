package importer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// TestImportedInventoryNameResolvesToTheStoredInventory covers what happens to the --inventory value
// an operator passes the crontab and Rundeck importers. Neither of those sources names an inventory
// file, so the caller supplies one, and the value landed in the field that holds a path on the
// server's filesystem. An operator naming their stored inventory, which is the obvious thing to type,
// got templates pointing at a file that does not exist, and found out when one fired.
//
// The name is resolved against the stored inventories when the plan is applied: a match wires the
// object by id, which is what the interface and the grant checks work in terms of, and anything else
// stays a path and says so.
func TestImportedInventoryNameResolvesToTheStoredInventory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()

	inventories := inventory.NewMemStore()
	stored := &inventory.Inventory{
		ID: "inv_stored", Name: "production", Content: "[all]\nweb-1\n", CreatedAt: now,
	}
	if err := inventories.Save(ctx, stored); err != nil {
		t.Fatalf("Save inventory: %v", err)
	}
	stores := func() importer.ApplyStores {
		return importer.ApplyStores{
			Projects: project.NewMemStore(), Inventories: inventories,
			Credentials: credential.NewMemStore(), Templates: template.NewMemStore(),
			Schedules: schedule.NewMemStore(),
		}
	}
	jobs := []byte(`[{"name":"rotate logs","sequence":{"commands":[{"exec":"/usr/local/bin/rotate"}]}}]`)

	plan, err := importer.FromRundeck("production")(jobs, now)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	if _, err := plan.Apply(ctx, stores()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := plan.Templates[0]; got.InventoryID != "inv_stored" {
		t.Errorf("imported template InventoryID = %q, want inv_stored: the name was left as a "+
			"filesystem path, so the template targets a file that does not exist", got.InventoryID)
	}

	// A value that names no stored inventory is a path, and the plan says so rather than leaving the
	// operator to discover it when the template launches.
	other, err := importer.FromRundeck("/etc/ansible/hosts")(jobs, now)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if _, err := other.Apply(ctx, stores()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if other.Templates[0].InventoryID != "" || other.Templates[0].Inventory != "/etc/ansible/hosts" {
		t.Errorf("path inventory = %+v, want it left as a path", other.Templates[0])
	}
	assertWarns(t, other.Warnings, "path on the server")
}

// TestCronImportSaysWhereTheCommandRuns covers the other half of the same confusion. A crontab line
// becomes a shell step, and a shell step runs on the SwitchTender host, not on the machine the crontab
// was taken from. Naming an inventory does not move it. An operator migrating a fleet of crontabs has
// to be told that, because the failure mode is a command running in the wrong place and succeeding.
func TestCronImportSaysWhereTheCommandRuns(t *testing.T) {
	t.Parallel()
	plan, err := importer.FromCron("production", false)(
		[]byte("0 2 * * * /usr/local/bin/rotate-logs\n"), time.Now())
	if err != nil {
		t.Fatalf("FromCron() error = %v", err)
	}
	assertWarns(t, plan.Warnings, "runs on the SwitchTender host")
}

// TestEmptyImportIsAFailureNotASuccess covers a document the importer does not recognize. Feeding one
// in produced an empty plan, printed a summary of zeros, and exited zero, so an operator who exported
// from the wrong endpoint or the wrong version was told their migration succeeded and had created
// nothing. An import that recognized nothing is a failed import.
func TestEmptyImportIsAFailureNotASuccess(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// A real AWX export, but of a shape this importer does not read: the objects sit under a key it
	// never looks at, so every list comes back empty.
	unknown := []byte(`{"results":[{"type":"job_template","name":"Deploy Web"}]}`)

	if _, err := importer.FromAWX(unknown, now); err == nil {
		t.Error("an unrecognized AWX document imported cleanly, so an operator is told a migration " +
			"that created nothing succeeded")
	} else if !strings.Contains(err.Error(), "nothing") {
		t.Errorf("AWX error = %v, want it to say nothing was recognized", err)
	}
	if _, err := importer.FromSemaphore(unknown, now); err == nil {
		t.Error("an unrecognized Semaphore document imported cleanly")
	}
	if _, err := importer.FromCron("", false)([]byte("# only comments\n\n"), now); err == nil {
		t.Error("a crontab with nothing in it imported cleanly")
	}

	// A document that does carry objects still imports.
	real := []byte(`{"projects":[{"name":"acme","templates":[{"name":"deploy","playbook":"site.yml"}]}]}`)
	if _, err := importer.FromSemaphore(real, now); err != nil {
		t.Errorf("a real export failed to import: %v", err)
	}
}
