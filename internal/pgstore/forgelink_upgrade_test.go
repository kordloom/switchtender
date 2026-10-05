package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/trigger"
)

// TestUpgradeAddsCommentDecisionsAndForgeLinks opens a database made before decisions recorded the
// comment they came from and before forge accounts could be linked, and checks the upgrade gives it
// both: a decision made from a comment keeps its comment, and a link can be made and found. It runs
// against a database of its own, because it damages the schema.
func TestUpgradeAddsCommentDecisionsAndForgeLinks(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	own := freshDatabase(t, shared)
	db, err := Open(own)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw := rawHandle(t, own)
	for _, stmt := range []string{
		"ALTER TABLE run_decisions DROP COLUMN comment",
		"DROP TABLE forge_links",
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
	ctx := context.Background()

	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	want := &decision.Record{ID: "aud_up", Kind: decision.KindDecision, DecisionID: "aud_up",
		RunID: "run_up", Verdict: "approved", At: at, Actor: "ops-admin",
		ActorType: "forge_comment", Comment: &decision.Comment{Forge: trigger.ProviderGitLab,
			APIURL: "https://gitlab.example.com/api/v4", Repository: "platform/infra",
			PullRequest: 3, CommentID: 11, AuthorID: 22, BodySHA256: "f00d"}}
	if err := upgraded.Decisions().Save(ctx, want); err != nil {
		t.Fatalf("Save() after the upgrade error = %v", err)
	}
	got, err := upgraded.Decisions().Get(ctx, "aud_up")
	if err != nil {
		t.Fatalf("Get() after the upgrade error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("decision after the upgrade mismatch (-want +got):\n%s", diff)
	}

	l := &forgelink.Link{ID: "fl_up", UserID: "usr_up", Provider: trigger.ProviderGitLab,
		APIURL: "https://gitlab.example.com/api/v4", ForgeUserID: 22, CreatedAt: at}
	if err := upgraded.ForgeLinks().Create(ctx, l); err != nil {
		t.Fatalf("Create() after the upgrade error = %v", err)
	}
	found, err := upgraded.ForgeLinks().Lookup(ctx, trigger.ProviderGitLab,
		"https://gitlab.example.com/api/v4", 22)
	if err != nil {
		t.Fatalf("Lookup() after the upgrade error = %v", err)
	}
	if diff := cmp.Diff(l, found); diff != "" {
		t.Errorf("link after the upgrade mismatch (-want +got):\n%s", diff)
	}
}
