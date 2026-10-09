package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

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

// TestUnsealedLineSaysWhatAnApplyWithoutAKeyLoses pins the notice an apply prints when no
// encryption key is set and the export carried something only a key can keep.
func TestUnsealedLineSaysWhatAnApplyWithoutAKeyLoses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// KeySet is whether the key was set without its salt.
		KeySet bool
		// Targets is how many notification targets wait for a secret the export carried.
		Targets int
		// Keys is how many provisioning callback keys the apply drops.
		Keys int
		// WantLine is the notice, or empty when there is nothing to say.
		WantLine string
	}{{ // Test 0: Nothing to seal, nothing to say.
		WantLine: "",
	}, { // Test 1: Targets alone, plural.
		Targets: 2,
		WantLine: "No encryption key is set, so 2 notification targets wait for the address or key " +
			"the export carried. Set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT " +
			"first to keep them. The server needs the same pair.",
	}, { // Test 2: Keys alone, singular.
		Keys: 1,
		WantLine: "No encryption key is set, so 1 provisioning callback key is dropped. Set " +
			"SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT first to keep them. The " +
			"server needs the same pair.",
	}, { // Test 3: Both.
		Targets: 1, Keys: 3,
		WantLine: "No encryption key is set, so 1 notification target waits for the address or key " +
			"the export carried and 3 provisioning callback keys are dropped. Set " +
			"SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT first to keep them. The " +
			"server needs the same pair.",
	}, { // Test 4: A key with no salt names the salt as what is missing.
		KeySet: true, Targets: 1,
		WantLine: "The encryption key is set without its salt, so 1 notification target waits for " +
			"the address or key the export carried. Set SWITCHTENDER_ENCRYPTION_KEY and " +
			"SWITCHTENDER_ENCRYPTION_SALT first to keep them. The server needs the same pair.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := unsealedLine(test.KeySet, test.Targets, test.Keys)
			if diff := cmp.Diff(test.WantLine, got); diff != "" {
				t.Errorf("unsealedLine(%t, %d, %d) mismatch (-want +got):\n%s", test.KeySet,
					test.Targets, test.Keys, diff)
			}
		})
	}
}

// sealedAWXExport is an AWX export carrying the two things only an encryption key can keep: a
// webhook address and a provisioning callback key. A second template holds a key with callbacks
// off, which no host presents, so the notice does not count it.
const sealedAWXExport = `{
  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://example.invalid/i.git"}],
  "inventory": [{"name": "fleet", "hosts": [{"name": "web01"}]}],
  "notification_templates": [
    {"name": "ops hook", "notification_type": "webhook",
     "notification_configuration": {"url": "https://hooks.example.invalid/ops", "headers": {},
       "http_method": "POST", "username": "", "password": "", "disable_ssl_verification": false}}
  ],
  "job_templates": [
    {"name": "boot", "playbook": "boot.yml", "project": "infra", "inventory": "fleet",
     "allow_callbacks": true, "host_config_key": "awx-callback-key-1234"},
    {"name": "quiet", "playbook": "quiet.yml", "project": "infra", "inventory": "fleet",
     "allow_callbacks": false, "host_config_key": "awx-callback-key-5678"}
  ]
}`

// conflictingAWXExport is an AWX export the apply refuses before it creates any object: two
// templates that take callbacks claim the same AWX id.
const conflictingAWXExport = `{
  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://example.invalid/i.git"}],
  "job_templates": [
    {"name": "a", "playbook": "a.yml", "project": "infra", "allow_callbacks": true, "id": 42},
    {"name": "b", "playbook": "b.yml", "project": "infra", "allow_callbacks": true, "id": 42}
  ]
}`

// TestImportApplySaysWhatNoKeyCosts pins the stderr notice of an apply run with no encryption key.
// The sealer's own warning goes to a no-op logger, so without this notice an apply with no key
// reads byte for byte the same as a keyed one, and the webhook address and the callback key the
// export carried are missed only when a run notifies nobody.
func TestImportApplySaysWhatNoKeyCosts(t *testing.T) {
	tests := []struct {
		// Key is the encryption key in the environment, or empty for none.
		Key string
		// Salt is the encryption salt in the environment, or empty for none.
		Salt string
		// WantStderr are fragments stderr must carry.
		WantStderr []string
		// WantAbsent are fragments neither stream may carry.
		WantAbsent []string
		// WantStdout are fragments stdout must carry.
		WantStdout []string
	}{{ // Test 0: No key, so the notice names what waits and what is dropped.
		WantStderr: []string{"No encryption key is set, so 1 notification target waits for the " +
			"address or key the export carried and 1 provisioning callback key is dropped."},
		WantStdout: []string{`notification target "ops hook" arrives waiting for its address`,
			`template "boot" accepts provisioning callbacks, and the key the export carried for it ` +
				"was dropped"},
		WantAbsent: []string{`template "quiet" accepts provisioning callbacks`},
	}, { // Test 1: A key with no salt seals nothing either, and the notice says the salt is missing.
		Key: "import-test-passphrase",
		WantStderr: []string{"The encryption key is set without its salt, so 1 notification " +
			"target waits"},
		WantAbsent: []string{"No encryption key", `template "quiet" accepts provisioning callbacks`},
	}, { // Test 2: With a key and a salt there is nothing to say.
		Key: "import-test-passphrase", Salt: "import-test-salt",
		WantAbsent: []string{"No encryption key", "was dropped", "arrives waiting"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: t.Setenv mutates the process environment and the import commands read
			// package-level flag variables.
			t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", test.Key)
			t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", test.Salt)
			dir := t.TempDir()
			export := filepath.Join(dir, "awx-export.json")
			if err := os.WriteFile(export, []byte(sealedAWXExport), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			setString(t, &importDB, filepath.Join(dir, "switchtender.db"))
			setBool(t, &importApply, true)

			var stdout, stderr bytes.Buffer
			c := testCommand()
			c.SetOut(&stdout)
			c.SetErr(&stderr)
			if err := runImport(c, export, importer.FromAWX); err != nil {
				t.Fatalf("runImport() error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(),
					stderr.String())
			}
			for _, want := range test.WantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr lacks %q.\nstderr:\n%s", want, stderr.String())
				}
			}
			for _, want := range test.WantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q.\nstdout:\n%s", want, stdout.String())
				}
			}
			for _, absent := range test.WantAbsent {
				if strings.Contains(stdout.String()+stderr.String(), absent) {
					t.Errorf("the import printed %q.\nstdout:\n%s\nstderr:\n%s",
						absent, stdout.String(), stderr.String())
				}
			}
		})
	}
}

// TestARefusedApplyStillSaysItStartedADatabase pins the notice on the error path. An apply the plan
// refuses has already created the SQLite file, with the audit entry of the attempt in it, so the
// notice is printed on that path too. A database left behind in silence is the wrong-directory trap
// the notice exists to prevent.
func TestARefusedApplyStillSaysItStartedADatabase(t *testing.T) {
	// Not parallel: the import commands read package-level flag variables.
	dir := t.TempDir()
	export := filepath.Join(dir, "awx-export.json")
	if err := os.WriteFile(export, []byte(conflictingAWXExport), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	db := filepath.Join(dir, "refused.db")
	setString(t, &importDB, db)
	setBool(t, &importApply, true)

	var stdout, stderr bytes.Buffer
	c := testCommand()
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	err := runImport(c, export, importer.FromAWX)
	if err == nil || !strings.Contains(err.Error(), "AWX job template id 42") {
		t.Fatalf("runImport() error = %v, want the id conflict refused", err)
	}
	if !fileExists(db) {
		t.Fatalf("the refused apply left no database at %s, so there is nothing to warn about", db)
	}
	want := "This started a new database even though the apply did not finish."
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr lacks %q.\nstderr:\n%s", want, stderr.String())
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

// TestImportHelpKeepsEachParagraphFilled pins the layout of every import command's long help. A
// paragraph reflowed by hand can leave one short line in the middle of a sentence, which --help
// prints as a ragged break that reads like a lost line.
func TestImportHelpKeepsEachParagraphFilled(t *testing.T) {
	t.Parallel()
	// minHelpLine is the shortest a line may run when another line of its paragraph follows it.
	const minHelpLine = 60
	for _, c := range importCmd.Commands() {
		t.Run(c.Name(), func(t *testing.T) {
			t.Parallel()
			for _, para := range strings.Split(c.Long, "\n\n") {
				lines := strings.Split(strings.TrimRight(para, "\n"), "\n")
				for _, line := range lines[:len(lines)-1] {
					if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
						continue
					}
					if len(line) < minHelpLine {
						t.Errorf("import %s --help breaks a paragraph after %q, %d characters in",
							c.Name(), line, len(line))
					}
				}
			}
		})
	}
}
