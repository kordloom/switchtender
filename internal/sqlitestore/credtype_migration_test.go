package sqlitestore_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestCredentialTypeStampSurvivesTheColumnRebuild pins the migration that moves
// credential_types.created_at from the UnixNano integer an older release wrote to the text form the
// reader now parses. Without it every read of the table fails after an upgrade, which is the whole
// credential-type catalog gone on a database that has one.
//
// The stamp has to come back as the instant it was written, not merely as something parseable: the
// list is ordered oldest first, and a rebuild that lost the times would silently reorder a catalog
// an operator navigates by.
func TestCredentialTypeStampSurvivesTheColumnRebuild(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credtypes.db")
	db := openStoreAt(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	want := time.Date(2025, time.March, 4, 5, 6, 7, 0, time.UTC)
	rawExec(t, path,
		"DROP TABLE credential_types",
		`CREATE TABLE credential_types (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	fields     TEXT NOT NULL DEFAULT '[]',
	env        TEXT NOT NULL DEFAULT '{}',
	extra_vars TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL DEFAULT 0
)`,
		"INSERT INTO credential_types (id, name, fields, env, extra_vars, created_at) VALUES "+
			"('ct_legacy', 'Datadog API', '[]', '{}', '{}', "+
			strconv.FormatInt(want.UnixNano(), 10)+")")

	healed := openStoreAt(t, path)
	got, err := healed.CredentialTypes().Get(ctx, "ct_legacy")
	if err != nil {
		t.Fatalf("Get() after the column rebuild error = %v, so an upgraded install cannot read "+
			"the credential types it already has", err)
	}
	if !got.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %s, want %s", got.CreatedAt, want)
	}

	list, err := healed.CredentialTypes().List(ctx)
	if err != nil {
		t.Fatalf("List() after the column rebuild error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() returned %d types, want 1", len(list))
	}
}
