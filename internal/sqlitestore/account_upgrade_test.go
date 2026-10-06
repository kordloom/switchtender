package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
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
