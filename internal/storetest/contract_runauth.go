package storetest

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// testRunAuthOutlivesTheRun holds every backend to the property the derived reads depend on.
//
// Summaries, drift, and host state history outlive the runs that produced them on purpose, and
// readability is decided by resolving the governing run. Retention deletes runs, so those two
// collide by design: ask far enough back and the decision has nothing to resolve, and every derived
// row the run governs becomes unreadable to a grant-restricted caller. Not refused, not explained,
// simply absent, at exactly the depth an audit asks about.
//
// Retaining the decision is what closes that, and it has to survive in all three backends
// identically or a Postgres install answers a different question from a SQLite one.
func testRunAuthOutlivesTheRun(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := store.RunAuthFor(ctx, "never_existed"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("RunAuthFor(unknown) error = %v, want ErrNotFound. A run nobody has heard of must "+
			"read as unreadable rather than as an empty decision that allows", err)
	}

	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := &run.Run{
		ID: "run_scoped", Status: run.StatusSucceeded, CreatedAt: old,
		Playbook: "site.yml", Inventory: "hosts.ini",
		OrgID: "org_1", ProjectID: "proj_1", InventoryID: "inv_1",
		PullCredentialID: "cred_pull", CredentialIDs: []string{"cred_a", "cred_b"},
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// While the run lives, the decision comes from the run.
	live, err := store.RunAuthFor(ctx, r.ID)
	if err != nil {
		t.Fatalf("RunAuthFor() on a live run error = %v", err)
	}
	assertRunAuth(t, "live", live, r)

	// Retention deletes the run.
	if _, err := store.PurgeRunsBefore(ctx, old.Add(24*time.Hour)); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if _, err := store.Get(ctx, r.ID); err == nil {
		t.Fatal("the run survived the purge, so this test is not exercising what it claims")
	}

	// The decision outlives it, and says exactly the same thing. Anything less scoped would widen
	// what a caller can read; anything more would hide what they could always see.
	retained, err := store.RunAuthFor(ctx, r.ID)
	if err != nil {
		t.Fatalf("RunAuthFor() after the purge error = %v. Every derived row this run governs is "+
			"now unreadable to a grant-restricted caller", err)
	}
	assertRunAuth(t, "retained", retained, r)
}

// assertRunAuth checks a decision carries exactly what scopes the run, no more and no less.
func assertRunAuth(t *testing.T, when string, got *run.RunAuth, want *run.Run) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s decision is nil", when)
	}
	if got.ID != want.ID || got.OrgID != want.OrgID || got.ProjectID != want.ProjectID ||
		got.InventoryID != want.InventoryID || got.PullCredentialID != want.PullCredentialID {
		t.Errorf("%s decision = %+v, want it to match the run's scoping fields", when, got)
	}
	gotCreds := append([]string{}, got.CredentialIDs...)
	wantCreds := append([]string{}, want.CredentialIDs...)
	sort.Strings(gotCreds)
	sort.Strings(wantCreds)
	if len(gotCreds) != len(wantCreds) {
		t.Fatalf("%s decision holds %d credentials, want %d: dropping one widens what the run is "+
			"scoped by", when, len(gotCreds), len(wantCreds))
	}
	for i := range gotCreds {
		if gotCreds[i] != wantCreds[i] {
			t.Errorf("%s credential %d = %q, want %q", when, i, gotCreds[i], wantCreds[i])
		}
	}
	// The object list is what every authorization path actually reads, so it is checked rather
	// than inferred from the fields above.
	if len(got.Objects()) != len(want.CredentialIDs)+3 {
		t.Errorf("%s decision scopes by %v, want every project, inventory, and credential the run "+
			"names", when, got.Objects())
	}
}

// testRunAuthIsCollectedOnlyWhenNothingNeedsIt holds every backend to both halves of the cleanup.
//
// A decision is written for every run retention deletes, and a busy fleet deletes runs forever, so
// they have to be collectable. But dropping one that still governs something is the failure the
// retaining exists to prevent, reintroduced from the other side: the rows do not error, they go
// quietly unreadable to every grant-restricted caller.
//
// So both directions are checked. Keeping too long costs a little disk. Dropping too early costs
// an audit answer.
func testRunAuthIsCollectedOnlyWhenNothingNeedsIt(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	run.SetFactsInterval(0)
	run.SetFactsDepth(0)
	t.Cleanup(func() {
		run.SetFactsInterval(run.DefaultFactsInterval)
		run.SetFactsDepth(run.DefaultFactsDepth)
	})

	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// One run leaves a reading behind; the other leaves nothing at all.
	watched := &run.Run{ID: "run_watched", Status: run.StatusSucceeded, CreatedAt: old,
		Playbook: "site.yml", ProjectID: "proj_1"}
	forgotten := &run.Run{ID: "run_forgotten", Status: run.StatusSucceeded, CreatedAt: old,
		Playbook: "site.yml", ProjectID: "proj_1"}
	for _, r := range []*run.Run{watched, forgotten} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}
	if err := store.SaveHostFacts(ctx, watched.ID, []run.HostFacts{{
		Host: "web01", Facts: map[string]string{"kernel": "6.1.0"}, GatheredAt: old,
	}}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}

	if _, err := store.PurgeRunsBefore(ctx, old.Add(24*time.Hour)); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	// Both decisions exist now, because both runs were purged.
	for _, id := range []string{watched.ID, forgotten.ID} {
		if _, err := store.RunAuthFor(ctx, id); err != nil {
			t.Fatalf("RunAuthFor(%s) after the purge error = %v", id, err)
		}
	}

	if _, err := store.PurgeRunAuth(ctx); err != nil {
		t.Fatalf("PurgeRunAuth() error = %v", err)
	}

	// The watched run's reading is still held, so its decision must survive. Dropping it would make
	// that reading invisible to exactly the callers the decision was kept for.
	if _, err := store.RunAuthFor(ctx, watched.ID); err != nil {
		t.Errorf("RunAuthFor(%s) error = %v after collection, but a host reading still names that "+
			"run. The reading is now unreadable to every grant-restricted caller", watched.ID, err)
	}
	// The forgotten run governs nothing, so its decision is collectable.
	if _, err := store.RunAuthFor(ctx, forgotten.ID); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("RunAuthFor(%s) error = %v, want ErrNotFound: a decision nothing references is "+
			"kept forever and the table only grows", forgotten.ID, err)
	}
}
