package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheAccountColumnsHealOnAnUpgrade opens a database from before a run and a policy carried an
// account, and holds the upgrade to keeping both: an exemption saved after it still names its
// account, and an agent's run still carries the account it was asked for under. Without the
// columns, every save would fail, since every save names them.
func TestTheAccountColumnsHealOnAnUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := openStoreAt(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rawExec(t, path, "ALTER TABLE runs DROP COLUMN account",
		"ALTER TABLE policies DROP COLUMN account")

	healed := openStoreAt(t, path)
	exempt := &policy.Policy{ID: "pol_ci", Name: "ci", Actor: "ci-agent", Account: "team-a",
		Effect: policy.EffectExempt, MaxDestroy: policy.DisabledMaxDestroy, CreatedAt: baseTime}
	if err := healed.Policies().Save(ctx, exempt); err != nil {
		t.Fatalf("Policies().Save() on a healed database error = %v", err)
	}
	r := &run.Run{ID: "run_ci", Playbook: "site.yml", Status: run.StatusPending,
		CreatedAt: baseTime, Actor: "ci-agent", ActorType: "agent", Account: "team-a"}
	if err := healed.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Runs().Save() on a healed database error = %v", err)
	}
	gotPolicy, err := healed.Policies().Get(ctx, exempt.ID)
	if err != nil {
		t.Fatalf("Policies().Get() error = %v", err)
	}
	gotRun, err := healed.Runs().Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Runs().Get() error = %v", err)
	}
	if gotPolicy.Account != "team-a" || gotRun.Account != "team-a" {
		t.Errorf("policy account = %q and run account = %q, want both team-a", gotPolicy.Account,
			gotRun.Account)
	}
}

// TestTheSignInIdentityColumnsHealOnAnUpgrade opens a database from before an account recorded the
// identity that created it. The account made then still reads, signs in by password, and keeps its
// role, with no stored identity, and an account saved after the upgrade keeps its pair. Without the
// columns every read fails, since every read selects them.
func TestTheSignInIdentityColumnsHealOnAnUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := openStoreAt(t, path)
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
	rawExec(t, path, "ALTER TABLE users DROP COLUMN issuer", "ALTER TABLE users DROP COLUMN subject")

	healed := openStoreAt(t, path)
	got, err := user.Authenticate(ctx, healed.Users(), "ada", "correct horse")
	if err != nil {
		t.Fatalf("Authenticate() on a healed database error = %v", err)
	}
	if got.Role != user.RoleOperator || got.Issuer != "" || got.Subject != "" {
		t.Errorf("old account = role %q, pair (%q, %q), want operator and no pair", got.Role,
			got.Issuer, got.Subject)
	}
	fresh := &user.User{ID: "user_new", Username: "grace", Role: user.RoleViewer, Source: "oidc",
		Issuer: "https://idp.example.com", Subject: "00u1a2b3c4", CreatedAt: baseTime}
	if err := healed.Users().Save(ctx, fresh); err != nil {
		t.Fatalf("Save() on a healed database error = %v", err)
	}
	back, err := healed.Users().Get(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if back.Issuer != fresh.Issuer || back.Subject != fresh.Subject {
		t.Errorf("new account pair = (%q, %q), want (%q, %q)", back.Issuer, back.Subject,
			fresh.Issuer, fresh.Subject)
	}
}
