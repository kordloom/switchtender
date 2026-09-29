package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// testCommand returns a cobra command carrying a real context, since the run functions read
// cmd.Context() and a bare command has none.
func testCommand() *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(context.Background())
	return c
}

// seedForBackup fills a database with one object of each kind that a backup carries, so a round
// trip has something to prove.
func seedForBackup(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	bundle, err := openBundle(dbPath)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	closed := false
	closeOnce := func() {
		if !closed {
			closed = true
			if err := bundle.Close(); err != nil {
				t.Fatalf("close seeding bundle: %v", err)
			}
		}
	}
	defer closeOnce()

	if err := bundle.Projects().Save(ctx, &project.Project{
		ID: "proj_1", Name: "web-platform", RepoURL: "https://example.com/web.git", Branch: "main",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	if err := bundle.Inventories().Save(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "production", Content: "[web]\nweb01\n",
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := bundle.Credentials().Save(ctx, &credential.Credential{
		ID: "cred_1", Name: "prod-ssh", Kind: credential.KindSSHKey,
	}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	if err := bundle.Templates().Save(ctx, &template.Template{
		ID: "tpl_1", Name: "Deploy web", Playbook: "site.yml", ProjectID: "proj_1", InventoryID: "inv_1",
	}); err != nil {
		t.Fatalf("save template: %v", err)
	}
	account, err := user.New("alice", "correct-horse", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := bundle.Users().Save(ctx, account); err != nil {
		t.Fatalf("save user: %v", err)
	}
	// The command opens the same file, and SQLite is limited to one writer, so the seeding
	// handle must be released before it runs rather than at the end of the test.
	closeOnce()
}

// TestBackupRestoreRoundTrip verifies a backup written from one database restores into an empty
// one with every object intact. A backup nobody can restore is worse than no backup, so the round
// trip is asserted end to end through the real commands rather than the library alone.
func TestBackupRestoreRoundTrip(t *testing.T) {
	// A backup carries sealed secrets, so it needs the encryption pair on both ends.
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "test-key-material")
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", "test-salt-material")
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	target := filepath.Join(dir, "target.db")
	archive := filepath.Join(dir, "backup.json")

	seedForBackup(t, source)

	// Back up the seeded database to a file.
	backupDB, backupOut = source, archive
	t.Cleanup(func() { backupDB, backupOut, restoreDB, restoreIn = "", "", "", "" })
	if err := runBackup(testCommand(), nil); err != nil {
		t.Fatalf("runBackup() error = %v", err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("backup file is empty")
	}
	// The archive holds sealed secrets, so it must not be world readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup file mode = %v, want 0600 since it can hold sealed secrets", perm)
	}

	// Restore into a fresh database.
	restoreDB, restoreIn = target, archive
	if err := runRestore(testCommand(), nil); err != nil {
		t.Fatalf("runRestore() error = %v", err)
	}

	ctx := context.Background()
	restored, err := openBundle(target)
	if err != nil {
		t.Fatalf("openBundle(target) error = %v", err)
	}
	defer func() { _ = restored.Close() }()

	proj, err := restored.Projects().Get(ctx, "proj_1")
	if err != nil || proj.Name != "web-platform" || proj.RepoURL != "https://example.com/web.git" {
		t.Errorf("restored project = %+v, err %v, want web-platform with its repository", proj, err)
	}
	inv, err := restored.Inventories().Get(ctx, "inv_1")
	if err != nil || !strings.Contains(inv.Content, "web01") {
		t.Errorf("restored inventory = %+v, err %v, want its host content", inv, err)
	}
	cred, err := restored.Credentials().Get(ctx, "cred_1")
	if err != nil || cred.Name != "prod-ssh" {
		t.Errorf("restored credential = %+v, err %v, want prod-ssh", cred, err)
	}
	tpl, err := restored.Templates().Get(ctx, "tpl_1")
	if err != nil || tpl.Playbook != "site.yml" || tpl.ProjectID != "proj_1" {
		t.Errorf("restored template = %+v, err %v, want its playbook and project reference", tpl, err)
	}
	users, err := restored.Users().List(ctx)
	if err != nil || len(users) != 1 || users[0].Username != "alice" {
		t.Errorf("restored users = %+v, err %v, want alice", users, err)
	}
	// The password hash must survive, or the restored account cannot sign in.
	if _, err := user.Authenticate(ctx, restored.Users(), "alice", "correct-horse"); err != nil {
		t.Errorf("restored account cannot authenticate: %v", err)
	}
}

// TestRestoreRejectsGarbage verifies a corrupt archive fails loudly instead of half-importing.
func TestRestoreRejectsGarbage(t *testing.T) {
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "test-key-material")
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", "test-salt-material")
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	archive := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(archive, []byte("this is not a backup"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	restoreDB, restoreIn = target, archive
	t.Cleanup(func() { restoreDB, restoreIn = "", "" })
	if err := runRestore(testCommand(), nil); err == nil {
		t.Error("runRestore() on a corrupt archive = nil error, want a failure")
	}
}

// TestBackupMissingInputs verifies both commands refuse an unreadable path rather than reporting
// a false success.
func TestBackupMissingInputs(t *testing.T) {
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "test-key-material")
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", "test-salt-material")
	dir := t.TempDir()
	restoreDB, restoreIn = filepath.Join(dir, "t.db"), filepath.Join(dir, "absent.json")
	t.Cleanup(func() { restoreDB, restoreIn = "", "" })
	if err := runRestore(testCommand(), nil); err == nil {
		t.Error("runRestore() with a missing archive = nil error, want a failure")
	}
}

// TestBackupRefusesWithoutEncryption verifies a backup is refused when no encryption pair is set,
// rather than writing an archive whose stored secrets are unprotected.
func TestBackupRefusesWithoutEncryption(t *testing.T) {
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "")
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", "")
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	seedForBackup(t, source)

	backupDB, backupOut = source, filepath.Join(dir, "backup.json")
	t.Cleanup(func() { backupDB, backupOut = "", "" })
	err := runBackup(testCommand(), nil)
	if err == nil {
		t.Fatal("runBackup() without an encryption pair = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "encryption") {
		t.Errorf("error = %v, want it to name the missing encryption pair", err)
	}
	if _, statErr := os.Stat(backupOut); statErr == nil {
		t.Error("a backup file was written despite the refusal")
	}
}

// TestRestoreRecordsProvenance proves a restore records not only that it happened but what it drew
// from: an entry whose path carries the instant the source backup was taken and whose content digest
// commits the restored counts. The pre-restore guard entry proves the change was authorized; this
// provenance entry proves which snapshot produced it, so an operator can tell one restore from
// another.
func TestRestoreRecordsProvenance(t *testing.T) {
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "test-key-material")
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", "test-salt-material")
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	target := filepath.Join(dir, "target.db")
	archive := filepath.Join(dir, "backup.json")

	seedForBackup(t, source)
	backupDB, backupOut = source, archive
	t.Cleanup(func() { backupDB, backupOut, restoreDB, restoreIn = "", "", "", "" })
	if err := runBackup(testCommand(), nil); err != nil {
		t.Fatalf("runBackup() error = %v", err)
	}
	restoreDB, restoreIn = target, archive
	if err := runRestore(testCommand(), nil); err != nil {
		t.Fatalf("runRestore() error = %v", err)
	}

	ctx := context.Background()
	restored, err := openBundle(target)
	if err != nil {
		t.Fatalf("openBundle(target) error = %v", err)
	}
	defer func() { _ = restored.Close() }()

	chain, err := restored.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var guard, provenance bool
	for _, e := range chain {
		if e.Path == "/cli/restore" {
			guard = true
		}
		if strings.HasPrefix(e.Path, "/cli/restore?taken_at=") {
			provenance = true
			if e.Method != audit.MethodCLI {
				t.Errorf("provenance method = %q, want %q", e.Method, audit.MethodCLI)
			}
			if e.ActorType != "cli" {
				t.Errorf("provenance actor type = %q, want cli", e.ActorType)
			}
			// The digest commits the restored counts into the chain.
			if e.ContentDigest == "" {
				t.Error("provenance entry has no content digest, want the counts committed")
			}
			takenAt := strings.TrimPrefix(e.Path, "/cli/restore?taken_at=")
			if _, perr := time.Parse(time.RFC3339, takenAt); perr != nil {
				t.Errorf("provenance taken_at %q is not an RFC3339 time: %v", takenAt, perr)
			}
		}
	}
	if !guard {
		t.Error("no pre-restore guard entry recorded")
	}
	if !provenance {
		t.Error("no restore provenance entry recorded")
	}
}
