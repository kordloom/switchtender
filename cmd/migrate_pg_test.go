package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/backup"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/template"
)

// TestBackupRestoreMovesSQLiteToPostgres proves the documented growth path end to end: back up a
// SQLite install and restore it into PostgreSQL through the same bundle wiring the CLI commands
// use. Restoring into an empty PostgreSQL database initializes the schema, which is the licensed
// act, so the test runs under a Team license and puts Community back when it ends. It does not run
// in parallel because the license is process-global.
func TestBackupRestoreMovesSQLiteToPostgres(t *testing.T) {
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN is not: " +
				"the full suite was demanded and the migration contract cannot run")
		}
		t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "migrate-test-key")
	t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", "migrate-test-salt")
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_migrate_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	t.Cleanup(func() { license.Set(nil) })
	ctx := context.Background()

	src, err := openBundle(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatalf("open sqlite source: %v", err)
	}
	defer func() { _ = src.Close() }()
	want := &template.Template{ID: "tpl_pgmigrate", Name: "pg migrate proof", Tool: "bash",
		Command: "echo moved"}
	if err := src.Templates().Save(ctx, want); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	var buf bytes.Buffer
	sealer := newSealerFromEnv(zap.NewNop())
	if _, err := backup.Write(ctx, backupStores(src), sealer, &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	dst, err := openBundle(dsn)
	if err != nil {
		t.Fatalf("open postgres destination: %v", err)
	}
	defer func() { _ = dst.Close() }()
	sum, err := backup.Read(ctx, backupStores(dst), sealer, &buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if sum.Templates == 0 {
		t.Fatalf("restore summary counted no templates: %+v", sum)
	}
	got, err := dst.Templates().Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("template after migration: %v", err)
	}
	if got.Name != want.Name || got.Tool != want.Tool || got.Command != want.Command {
		t.Errorf("migrated template = %q %q %q, want %q %q %q",
			got.Name, got.Tool, got.Command, want.Name, want.Tool, want.Command)
	}
}
