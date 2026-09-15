package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// journeyBinary builds the CLI once for the whole package and returns its path, or the reason it
// could not be built.
var journeyBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "switchtender-journey-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "switchtender")
	build := exec.Command("go", "build", "-o", bin, "..")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		return "", &exec.ExitError{Stderr: out}
	}
	return bin, nil
})

// TestEveryImportVerbCreatesObjects walks the migration journey five marketing pages sell, through
// the command a person actually types.
//
// The importer package has good fixtures and thorough unit tests. What nothing exercised was the
// verb end to end: parse the export, open a store, write the objects, and report. That is the whole
// of what a switching evaluator does on their first afternoon, and it was covered only by a test
// asserting the command exists.
//
// It runs a real binary rather than calling RunE in this process, for two reasons. It is the path a
// person uses, so it covers flag parsing and exit codes as well as the import. And opening and
// closing the SQLite store repeatedly inside one process deadlocks in splitDB.Close, which is a
// pre-existing defect unrelated to importing and would make this test hang rather than report.
func TestEveryImportVerbCreatesObjects(t *testing.T) {
	t.Parallel()
	bin, err := journeyBinary()
	if err != nil {
		t.Fatalf("build the CLI: %v", err)
	}
	fixtures := filepath.Join("..", "internal", "importer", "testdata")

	tests := []struct {
		// Name is the verb as typed.
		Name string
		// Fixture is the export to import, relative to the importer's testdata.
		Fixture string
		// WantCreates is the minimum number of objects the apply must report creating. A floor
		// rather than an exact count, so enriching a fixture does not fail the journey.
		WantCreates int
	}{
		{Name: "awx", Fixture: "awx-export.json", WantCreates: 1},
		{Name: "semaphore", Fixture: "semaphore-export.json", WantCreates: 1},
		{Name: "rundeck", Fixture: "rundeck-archive-file-nodesource.zip", WantCreates: 1},
		{Name: "jenkins", Fixture: "jenkins-home", WantCreates: 1},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			source := filepath.Join(fixtures, test.Fixture)
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("%s: fixture missing: %v", test.Name, err)
			}
			db := filepath.Join(t.TempDir(), "import.db")

			run := func(args ...string) string {
				t.Helper()
				cmd := exec.Command(bin, args...)
				cmd.Env = append(os.Environ(),
					"SWITCHTENDER_ENCRYPTION_KEY=journey", "SWITCHTENDER_ENCRYPTION_SALT=journey")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v failed: %v\n%s", test.Name, args, err, out)
				}
				return string(out)
			}

			// The dry run first, which is what the docs tell a reader to do before committing.
			preview := run("import", test.Name, source, "--db", db)
			if strings.TrimSpace(preview) == "" {
				t.Errorf("%s: the dry run reported nothing, so a reader cannot see what would be "+
					"created before committing to it", test.Name)
			}
			if _, err := os.Stat(db); err == nil {
				t.Errorf("%s: the dry run created a database, so it was not a dry run", test.Name)
			}

			applied := run("import", test.Name, source, "--db", db, "--apply")
			if _, err := os.Stat(db); err != nil {
				t.Fatalf("%s: apply wrote no database: %v", test.Name, err)
			}
			// The apply has to say what it did. A silent success on a migration is indistinguishable
			// from a no-op, which is exactly what the earlier Sunday-cron bug looked like.
			if !strings.Contains(strings.ToLower(applied), "creat") {
				t.Errorf("%s: apply did not report what it created:\n%s", test.Name, applied)
			}
		})
	}
}
