package sqlitestore_test

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTouchStampsOnlyTheTokenItNames pins that recording one token's use is scoped to that token.
//
// Touch is the only write on the hot authentication path, so it runs on every authenticated
// request. A predicate that matched more than the named id would restamp every token in the table
// on each request, which destroys the last-used column wholesale: an operator auditing which
// credentials are still live sees every token used seconds ago, so a key that has been dormant for
// a year is indistinguishable from the one actually in use and nothing stale is ever revoked.
// The existing coverage only proves Touch cannot resurrect a revoked token, which a statement that
// updates every surviving row still satisfies.
func TestTouchStampsOnlyTheTokenItNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Tokens()

	tokens := []*auth.Token{
		{ID: "tok_ci", Name: "ci", Hash: "h_ci", CreatedAt: baseTime},
		{ID: "tok_dormant", Name: "dormant", Hash: "h_dormant", CreatedAt: baseTime},
		{ID: "tok_other", Name: "other", Hash: "h_other", CreatedAt: baseTime},
	}
	for _, tok := range tokens {
		if err := store.Save(ctx, tok); err != nil {
			t.Fatalf("Save(%s) error = %v", tok.ID, err)
		}
	}

	used := baseTime.Add(time.Hour)
	if err := store.Touch(ctx, "tok_ci", used); err != nil {
		t.Fatalf("Touch() error = %v", err)
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != len(tokens) {
		t.Fatalf("List() returned %d tokens, want %d", len(got), len(tokens))
	}
	for _, tok := range got {
		switch tok.ID {
		case "tok_ci":
			if tok.LastUsedAt == nil || !tok.LastUsedAt.Equal(used) {
				t.Errorf("the touched token records last use %v, want %v", tok.LastUsedAt, used)
			}
		default:
			if tok.LastUsedAt != nil {
				t.Errorf("touching tok_ci also stamped %s with %v: every token now looks freshly "+
					"used, so a dormant credential reads as live and nobody revokes it",
					tok.ID, tok.LastUsedAt)
			}
		}
	}
}

// TestSweepNamesAParentlessRunInEitherStoredForm pins which swept runs the sweep reports back.
//
// The names it returns are what the caller commits to the audit chain, so a run the sweep settled
// but did not name ends terminal with no evidence at all. A child is deliberately left out because
// its outcome rolls up into its parent, and a child is recognized by carrying a parent. "No parent"
// has two stored forms: the column holds NULL, and it holds the empty string, because Save writes
// whatever the caller's pointer addresses and the claim predicate reads the two as equivalent with
// COALESCE. A report that only accepted NULL would treat a parentless run whose id came through as
// an empty string as somebody's child and stay silent about settling it, and nothing downstream
// would ever notice the missing outcome.
func TestSweepNamesAParentlessRunInEitherStoredForm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	stale := time.Now().Add(-10 * time.Minute)
	emptyParent := ""
	realParent := "run_parent"
	runs := []*run.Run{
		// Parentless with a NULL parent_id, the ordinary form.
		{ID: "run_top_null_parent", Playbook: "site.yml", Status: run.StatusRunning,
			CreatedAt: stale, ClaimedBy: "worker-that-died", ClaimedAt: &stale},
		// Parentless with an empty parent_id, the other stored form of the same fact.
		{ID: "run_top_empty_parent", Playbook: "site.yml", Status: run.StatusRunning,
			CreatedAt: stale, ClaimedBy: "worker-that-died", ClaimedAt: &stale,
			ParentID: &emptyParent},
		// A genuine child, which the sweep settles but must not name.
		{ID: "run_child", Playbook: "site.yml", Status: run.StatusRunning,
			CreatedAt: stale, ClaimedBy: "worker-that-died", ClaimedAt: &stale,
			ParentID: &realParent},
	}
	for _, r := range runs {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	reporter, ok := store.(interface {
		ReclaimStaleSettled(context.Context, time.Duration) (int, []string, error)
	})
	if !ok {
		t.Fatal("the store cannot name what its sweep settled, so swept runs leave no evidence")
	}
	_, settled, err := reporter.ReclaimStaleSettled(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("ReclaimStaleSettled() error = %v", err)
	}

	named := map[string]bool{}
	for _, id := range settled {
		named[id] = true
	}
	if !named["run_top_empty_parent"] {
		t.Errorf("settled = %v: the sweep settled run_top_empty_parent but did not name it, so a "+
			"parentless run whose parent_id is the empty string ends interrupted with no outcome "+
			"on the chain and no parent that will ever report it", settled)
	}
	if !named["run_top_null_parent"] {
		t.Errorf("settled = %v, missing run_top_null_parent: a swept top-level run left no evidence",
			settled)
	}
	if named["run_child"] {
		t.Errorf("settled = %v: a genuine child was named, so its outcome is committed twice, "+
			"once here and once by the parent rolling it up", settled)
	}
	if len(settled) != 2 {
		t.Errorf("settled = %v, want exactly the two parentless runs", settled)
	}
}
