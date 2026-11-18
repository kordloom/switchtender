package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/user"
)

// TestUpgradeAddsTheSignInIdentityColumns opens a database made before an account recorded the
// identity that created it, and checks the upgrade gives it both columns: the account made then still
// reads, signs in by password, and keeps its role with no stored identity, and an account saved after
// the upgrade keeps its pair. It runs against a database of its own, because it damages the schema.
func TestUpgradeAddsTheSignInIdentityColumns(t *testing.T) {
	t.Parallel()
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	own := freshDatabase(t, shared)
	db, err := Open(own)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ctx := context.Background()
	old, err := user.New("ada", "correct horse", user.RoleOperator)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := db.Users().Save(ctx, old); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw := rawHandle(t, own)
	for _, stmt := range []string{
		"ALTER TABLE users DROP COLUMN issuer",
		"ALTER TABLE users DROP COLUMN subject",
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("simulate the old schema, %s: %v", stmt, err)
		}
	}

	upgraded, err := Open(own)
	if err != nil {
		t.Fatalf("reopen the old database: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	got, err := user.Authenticate(ctx, upgraded.Users(), "ada", "correct horse")
	if err != nil {
		t.Fatalf("Authenticate() after the upgrade error = %v", err)
	}
	if got.Role != user.RoleOperator || got.Issuer != "" || got.Subject != "" {
		t.Errorf("old account = role %q, pair (%q, %q), want operator and no pair", got.Role,
			got.Issuer, got.Subject)
	}
	fresh := &user.User{ID: "usr_up", Username: "grace", Role: user.RoleViewer, Source: "oidc",
		Issuer: "https://idp.example.com", Subject: "00u1a2b3c4",
		CreatedAt: time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)}
	if err := upgraded.Users().Save(ctx, fresh); err != nil {
		t.Fatalf("Save() after the upgrade error = %v", err)
	}
	back, err := upgraded.Users().Get(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("Get() after the upgrade error = %v", err)
	}
	if back.Issuer != fresh.Issuer || back.Subject != fresh.Subject {
		t.Errorf("new account pair = (%q, %q), want (%q, %q)", back.Issuer, back.Subject,
			fresh.Issuer, fresh.Subject)
	}
}
