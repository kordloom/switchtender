package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/importer"
)

// TestCreatedLineSaysOnlyWhatTheImportNeeds pins the import's closing line. It told every import to
// re-enter credential secrets, including a crontab or a Chef fleet that creates no credentials, and
// said "1 objects".
func TestCreatedLineSaysOnlyWhatTheImportNeeds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Created, NeedSecret int
		WantLine            string
	}{{ // Test 0: No credentials, no instruction about them.
		Created: 12, WantLine: "Created 12 objects.",
	}, { // Test 1: One object is singular.
		Created: 1, WantLine: "Created 1 object.",
	}, { // Test 2: Credentials without secrets are counted.
		Created: 9, NeedSecret: 3,
		WantLine: "Created 9 objects. 3 credentials have no secret yet: enter it before running a template that needs it.",
	}, { // Test 3: One credential is singular.
		Created: 4, NeedSecret: 1,
		WantLine: "Created 4 objects. 1 credential has no secret yet: enter it before running a template that needs it.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := createdLine(test.Created, test.NeedSecret); got != test.WantLine {
				t.Errorf("createdLine(%d, %d) = %q, want %q", test.Created, test.NeedSecret, got, test.WantLine)
			}
		})
	}
}

// TestImportReportGoesToStdout pins which stream carries the import report. The report is what the
// command produces, the thing an operator saves and reads before letting an import write anything,
// and all of it went to stderr, so `import awx export.json > plan.txt` saved an empty file while
// the plan scrolled past on the terminal. The report goes to stdout, and only the note on what to
// do next goes to stderr. It also pins two lines of the report a crontab import got wrong: one
// object came across as "1 objects", and an import that brings no credentials was told their
// secrets must be re-entered.
func TestImportReportGoesToStdout(t *testing.T) {
	tests := []struct {
		Apply      bool
		WantStdout []string
		WantStderr []string
		WantAbsent []string
	}{{ // Test 0: A preview prints the summary and the plan, and says how to apply it.
		WantStdout: []string{"Migration summary:", "Import plan:", "(0 3 * * *)",
			"Comes across:       1 object\n", "Credentials: 0\n"},
		WantStderr: []string{"Run again with --apply"},
		WantAbsent: []string{"1 objects", "secrets must be re-entered"},
	}, { // Test 1: An apply prints the plan and what it created, and notes the new database.
		Apply:      true,
		WantStdout: []string{"Import plan:", "Created 1 object."},
		WantStderr: []string{"This started a new database"},
		WantAbsent: []string{"1 objects"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the import commands read package-level flag variables.
			dir := t.TempDir()
			crontab := filepath.Join(dir, "crontab")
			line := []byte("0 3 * * * /usr/local/bin/nightly-backup\n")
			if err := os.WriteFile(crontab, line, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			setString(t, &importDB, filepath.Join(dir, "switchtender.db"))
			setBool(t, &importApply, test.Apply)
			setString(t, &importCronInventory, "prod")
			setBool(t, &importCronSystem, false)

			var stdout, stderr bytes.Buffer
			c := testCommand()
			c.SetOut(&stdout)
			c.SetErr(&stderr)
			mapper := importer.FromCron(importCronInventory, importCronSystem)
			if err := runImport(c, crontab, mapper); err != nil {
				t.Fatalf("runImport() error = %v", err)
			}
			for _, want := range test.WantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q.\nstdout:\n%s\nstderr:\n%s", want, stdout.String(),
						stderr.String())
				}
				if strings.Contains(stderr.String(), want) {
					t.Errorf("stderr repeats %q, which belongs to the report on stdout", want)
				}
			}
			for _, want := range test.WantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr lacks %q.\nstderr:\n%s", want, stderr.String())
				}
				if strings.Contains(stdout.String(), want) {
					t.Errorf("stdout carries %q, a note to the operator, not the report", want)
				}
			}
			for _, absent := range test.WantAbsent {
				if strings.Contains(stdout.String()+stderr.String(), absent) {
					t.Errorf("the import printed %q.\nstdout:\n%s", absent, stdout.String())
				}
			}
		})
	}
}
