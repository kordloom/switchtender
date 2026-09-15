package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// TestEveryImportVerbCreatesObjects walks the migration journey five marketing pages sell, through
// the command a person actually types.
//
// The importer package has good fixtures and thorough unit tests. What nothing exercised was the
// CLI verb end to end: parse the export, open a store, and write the objects. That is the whole of
// what a switching evaluator does on their first afternoon, and it was covered only by a test
// asserting the command exists.
//
// This runs each verb against the fixture that package already maintains, applies it to a throwaway
// database, and counts what landed. A verb that stops creating anything fails here rather than in
// front of somebody migrating off AWX.
func TestEveryImportVerbCreatesObjects(t *testing.T) {
	fixtures := filepath.Join("..", "internal", "importer", "testdata")
	tests := []struct {
		// Name is the verb as typed.
		Name string
		// Fixture is the export to import, relative to the importer's testdata.
		Fixture string
		// WantTemplates and WantSchedules are the minimum each import must create. They are floors
		// rather than exact counts so enriching a fixture does not fail the journey.
		WantTemplates int
		WantSchedules int
	}{
		{Name: "awx", Fixture: "awx-export.json", WantTemplates: 1, WantSchedules: 1},
		{Name: "semaphore", Fixture: "semaphore-export.json", WantTemplates: 1, WantSchedules: 1},
		{Name: "rundeck", Fixture: "rundeck-archive-file-nodesource.zip", WantTemplates: 1, WantSchedules: 1},
		{Name: "jenkins", Fixture: "jenkins-home", WantTemplates: 1, WantSchedules: 1},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			// Not parallel: the import flags are package-level state shared by every verb.
			dbPath := filepath.Join(t.TempDir(), "import.db")
			source := filepath.Join(fixtures, test.Fixture)
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("%s: fixture missing: %v", test.Name, err)
			}

			origDB, origApply := importDB, importApply
			t.Cleanup(func() { importDB, importApply = origDB, origApply })

			// The dry run first, which is what the docs tell a reader to do before committing.
			importDB, importApply = dbPath, false
			rootCmd.SetArgs([]string{"import", test.Name, source, "--db", dbPath})
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("%s: dry run failed: %v", test.Name, err)
			}

			importDB, importApply = dbPath, true
			rootCmd.SetArgs([]string{"import", test.Name, source, "--db", dbPath, "--apply"})
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("%s: apply failed: %v", test.Name, err)
			}

			bundle, err := openBundle(dbPath)
			if err != nil {
				t.Fatalf("%s: open the imported store: %v", test.Name, err)
			}
			defer func() { _ = bundle.Close() }()

			ctx := context.Background()
			var templates []*template.Template
			if bundle.Templates() != nil {
				templates, err = bundle.Templates().List(ctx)
				if err != nil {
					t.Fatalf("%s: list templates: %v", test.Name, err)
				}
			}
			var schedules []*schedule.Schedule
			if bundle.Schedules() != nil {
				schedules, err = bundle.Schedules().List(ctx)
				if err != nil {
					t.Fatalf("%s: list schedules: %v", test.Name, err)
				}
			}
			if len(templates) < test.WantTemplates {
				t.Errorf("%s: created %d templates, want at least %d: the migration this verb "+
					"advertises brought nothing across", test.Name, len(templates), test.WantTemplates)
			}
			if len(schedules) < test.WantSchedules {
				t.Errorf("%s: created %d schedules, want at least %d", test.Name,
					len(schedules), test.WantSchedules)
			}
			// Every created object must be nameable, or the imported list reads as blank rows.
			for i, tpl := range templates {
				if tpl.Name == "" {
					t.Errorf("%s: template %d came across with no name", test.Name, i)
				}
			}
		})
	}
}
