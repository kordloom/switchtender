package sqlitestore_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAnchorShapeIsRestoredByItsHandMigration pins the one column the schema-derived healer cannot
// reach. The healer adds a missing column with the type and default the CREATE declares, and skips
// a column ALTER cannot add, which means one declared NOT NULL with no default. audit_anchors.shape
// is exactly that, and it arrived after the table shipped, so a database from before it is
// upgradable only through the hand-written ALTER that supplies a default. Without that ALTER the
// open dies on a column every anchor read selects.
func TestAnchorShapeIsRestoredByItsHandMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "anchors.db")

	db := openStoreAt(t, path)
	anchors, ok := db.Audits().(audit.AnchorStore)
	if !ok {
		t.Fatal("the audit store does not persist anchors")
	}
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_old", Type: audit.AnchorRFC3161, Shape: audit.AnchorShapeTree, Seq: 5,
		Link: "root", At: baseTime, Ref: "https://freetsa.org/tsr",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// A database from before the shape and install columns existed.
	rawExec(t, path,
		"ALTER TABLE audit_anchors DROP COLUMN shape",
		"ALTER TABLE audit_anchors DROP COLUMN install_id")

	healed := openStoreAt(t, path)
	healedAnchors, ok := healed.Audits().(audit.AnchorStore)
	if !ok {
		t.Fatal("the reopened audit store does not persist anchors")
	}
	got, err := healedAnchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors() after the upgrade error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Anchors() returned %d, want the stored anchor kept", len(got))
	}
	// A pre-shape anchor has no recorded coordinate space, and the migration's default says linear,
	// which is the only reading available: tree anchors did not exist before the column did.
	if got[0].Shape != audit.AnchorShapeLinear {
		t.Errorf("an anchor from before the shape column reads shape %q, want the migration's "+
			"default of %q", got[0].Shape, audit.AnchorShapeLinear)
	}
	if got[0].InstallID != "" {
		t.Errorf("an anchor from before the install column reads install %q, want empty",
			got[0].InstallID)
	}

	// A new anchor written after the upgrade records its own shape and install.
	if err := healedAnchors.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_new", Type: audit.AnchorGit, Shape: audit.AnchorShapeTree, Seq: 20,
		Link: "root2", At: baseTime.Add(time.Hour), Ref: "https://example.com/commit/abc",
		InstallID: "in_abc",
	}); err != nil {
		t.Fatalf("SaveAnchor() after the upgrade error = %v", err)
	}
	got, err = healedAnchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}
	if len(got) != 2 || got[1].Shape != audit.AnchorShapeTree || got[1].InstallID != "in_abc" {
		t.Errorf("the anchor written after the upgrade = %+v, want its shape and install kept",
			got[1])
	}
}

// TestPolicyDistinctApproverSurvivesAnUpgrade pins the separation-of-duties column across the
// upgrade path. A rule loaded from a database that predates the column came back with the
// requirement off, which meant the person who requested a change could approve it themselves. The
// column defaults to off for a rule written before it existed, which is the honest reading, but a
// rule saved after the upgrade has to keep what the operator set.
func TestPolicyDistinctApproverSurvivesAnUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policies.db")

	db := openStoreAt(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rawExec(t, path,
		"ALTER TABLE policies DROP COLUMN distinct_approver",
		"INSERT INTO policies (id, name, tool, command_contains, inventory_id, queue, "+
			"exclude_dry_run, max_destroy, actor_kind, actor, min_risk, effect, created_at) "+
			"VALUES ('pol_old','legacy','terraform','','','',0,-1,'','','','hold',"+
			"'2026-07-01T00:00:00Z')")

	healed := openStoreAt(t, path)
	old, err := healed.Policies().Get(ctx, "pol_old")
	if err != nil {
		t.Fatalf("Get(pol_old) after the upgrade error = %v", err)
	}
	if old.RequireDistinctApprover {
		t.Error("a rule from before the column came back demanding a distinct approver, which " +
			"nobody configured")
	}

	// A rule written after the upgrade keeps the requirement through a save and a reopen, which is
	// the case that matters: the decision is usually made by a different process from the one that
	// wrote the rule.
	if err := healed.Policies().Save(ctx, &policy.Policy{
		ID: "pol_new", Name: "prod apply", Tool: "terraform", MaxDestroy: -1,
		Effect: "hold", RequireDistinctApprover: true, CreatedAt: baseTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := healed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened := openStoreAt(t, path)
	got, err := reopened.Policies().Get(ctx, "pol_new")
	if err != nil {
		t.Fatalf("Get(pol_new) error = %v", err)
	}
	if !got.RequireDistinctApprover {
		t.Error("the stored rule lost its distinct-approver requirement, so a requester can " +
			"approve their own change through any process that reads it back")
	}
}

// TestCancelPendingTakesOnlyAnUnclaimedWaitingRun pins the atomic cancel of work that has not
// started. It is the only path that ends a run outright rather than asking its holder to stop, so
// it must never take a run somebody is executing: canceling that one in the database leaves a
// process still running a playbook against real hosts with no record that it is doing so.
func TestCancelPendingTakesOnlyAnUnclaimedWaitingRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	tests := []struct {
		Name       string
		Status     run.Status
		ClaimedBy  string
		WantCancel bool
	}{{ // Test 0: An unclaimed pending run is cancelable.
		Name: "pending unclaimed", Status: run.StatusPending, WantCancel: true,
	}, { // Test 1: A run held for approval is cancelable, since nothing has started it.
		Name: "held unclaimed", Status: run.StatusPendingApproval, WantCancel: true,
	}, { // Test 2: A pending run somebody already leased is not, because a worker holds it.
		Name: "pending claimed", Status: run.StatusPending, ClaimedBy: "worker-1",
	}, { // Test 3: A running run is not, because it is executing.
		Name: "running", Status: run.StatusRunning, ClaimedBy: "worker-1",
	}, { // Test 4: A run that already finished is not, because it has an outcome.
		Name: "succeeded", Status: run.StatusSucceeded,
	}, { // Test 5: An interrupted run is not.
		Name: "interrupted", Status: run.StatusInterrupted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_%d", testNum)
			saveRuns(t, store, &run.Run{ID: id, Status: test.Status, CreatedAt: baseTime,
				ClaimedBy: test.ClaimedBy})
			canceled, err := store.CancelPending(ctx, id)
			if err != nil {
				t.Fatalf("CancelPending(%s) error = %v", test.Name, err)
			}
			if canceled != test.WantCancel {
				t.Fatalf("CancelPending(%s) = %v, want %v", test.Name, canceled, test.WantCancel)
			}
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !test.WantCancel {
				if got.Status != test.Status {
					t.Errorf("%s: the refused cancel changed the status to %q", test.Name,
						got.Status)
				}
				return
			}
			if got.Status != run.StatusCanceled {
				t.Errorf("%s: status = %q, want canceled", test.Name, got.Status)
			}
			if got.EndedAt == nil {
				t.Error("the canceled run records no end, so it has no place in history")
			}
			// Canceling twice is a lost race rather than an error, since another process may
			// have taken it first.
			again, err := store.CancelPending(ctx, id)
			if err != nil || again {
				t.Errorf("%s: a second CancelPending = (%v, %v), want a quietly lost race",
					test.Name, again, err)
			}
		})
	}

	// A run that does not exist is a lost race too, not an error, since it may have been purged.
	if canceled, err := store.CancelPending(ctx, "run_ghost"); err != nil || canceled {
		t.Errorf("CancelPending(missing) = (%v, %v), want a quietly lost race", canceled, err)
	}
}

// TestTransitionStatusIsAPureCompareAndSwap pins the narrow status move. It carries no side effects
// at all, so it can be used where a caller has already written everything else, and it has to
// refuse when the stored status is not the one the caller believes, because that belief is what
// makes the move safe.
func TestTransitionStatusIsAPureCompareAndSwap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Runs()

	tests := []struct {
		Name     string
		Stored   run.Status
		From     run.Status
		To       run.Status
		WantMove bool
	}{{ // Test 0: The expected status moves.
		Name: "match", Stored: run.StatusPendingApproval, From: run.StatusPendingApproval,
		To: run.StatusPending, WantMove: true,
	}, { // Test 1: A different stored status is refused.
		Name: "mismatch", Stored: run.StatusRunning, From: run.StatusPendingApproval,
		To: run.StatusPending,
	}, { // Test 2: Moving a run to the status it already holds still reports a change, because a
		// row was matched and written.
		Name: "same status", Stored: run.StatusPending, From: run.StatusPending,
		To: run.StatusPending, WantMove: true,
	}, { // Test 3: A rejection from held, which is how an approver denies a change.
		Name: "reject", Stored: run.StatusPendingApproval, From: run.StatusPendingApproval,
		To: run.StatusRejected, WantMove: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("run_%d", testNum)
			saveRuns(t, store, &run.Run{ID: id, Status: test.Stored, CreatedAt: baseTime,
				ClaimedBy: "holder", Error: "kept"})
			moved, err := store.TransitionStatus(ctx, id, test.From, test.To)
			if err != nil {
				t.Fatalf("TransitionStatus(%s) error = %v", test.Name, err)
			}
			if moved != test.WantMove {
				t.Fatalf("TransitionStatus(%s) = %v, want %v", test.Name, moved, test.WantMove)
			}
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			want := test.Stored
			if test.WantMove {
				want = test.To
			}
			if got.Status != want {
				t.Errorf("%s: status = %q, want %q", test.Name, got.Status, want)
			}
			// The swap owns the status column and nothing else.
			if got.ClaimedBy != "holder" || got.Error != "kept" || got.EndedAt != nil {
				t.Errorf("%s: the swap touched columns it does not own: %+v", test.Name, got)
			}
		})
	}

	// A run that does not exist reports no change rather than an error.
	if moved, err := store.TransitionStatus(ctx, "run_ghost", run.StatusPending,
		run.StatusRunning); err != nil || moved {
		t.Errorf("TransitionStatus(missing) = (%v, %v), want no change and no error", moved, err)
	}
}

// TestUpgradedDatabaseStillClaimsAndSweeps pins that the run indexes created after the column
// migrations really do cover a healed database. The claim index and the lease index name columns
// added long after the table shipped, so building them before the migrations would fail on a
// database from before those columns, and building them at all against a healed table is what
// proves the healing landed in the right order.
func TestUpgradedDatabaseStillClaimsAndSweeps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")

	db := openStoreAt(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// Strip the indexes and the columns they cover, which is the state a database predating them
	// is in.
	rawExec(t, path,
		"DROP INDEX IF EXISTS idx_runs_pending_claim",
		"DROP INDEX IF EXISTS idx_runs_leased",
		"DROP INDEX IF EXISTS idx_runs_actor",
		"DROP INDEX IF EXISTS idx_runs_source",
		"DROP INDEX IF EXISTS idx_runs_status_parent",
		"ALTER TABLE runs DROP COLUMN queue",
		"ALTER TABLE runs DROP COLUMN claim_secret",
		"ALTER TABLE runs DROP COLUMN actor",
		"ALTER TABLE runs DROP COLUMN source",
		"ALTER TABLE runs DROP COLUMN source_id")

	healed := openStoreAt(t, path)
	store := healed.Runs()

	stale := time.Now().Add(-time.Hour)
	saveRuns(t, store,
		&run.Run{ID: "run_claimable", Status: run.StatusPending, CreatedAt: baseTime,
			Queue: "", Actor: "sam", Source: "api"},
		&run.Run{ID: "run_stale", Status: run.StatusRunning, CreatedAt: baseTime,
			ClaimedBy: "dead", ClaimedAt: &stale},
	)

	claimed, err := store.Claim(ctx, "worker-1", nil)
	if err != nil {
		t.Fatalf("Claim() on a healed database error = %v", err)
	}
	if claimed.ID != "run_claimable" || claimed.ClaimSecret == "" {
		t.Errorf("Claim() = %+v, want the pending run with a fresh capability", claimed)
	}
	swept, err := store.ReclaimStale(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStale() on a healed database error = %v", err)
	}
	if swept == 0 {
		t.Error("the sweep found nothing on a healed database, so the lease columns did not heal")
	}
	got, err := store.Get(ctx, "run_stale")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("the stale run is %q, want interrupted", got.Status)
	}
	// The filters the healed indexes back still answer.
	if hits, err := store.ListPage(ctx, run.ListFilter{Actor: "sam"}, 0, 0); err != nil ||
		len(hits) != 1 {
		t.Errorf("ListPage(actor) on a healed database = (%d, %v), want one run", len(hits), err)
	}
	if hits, err := store.ListPage(ctx, run.ListFilter{Source: "api"}, 0, 0); err != nil ||
		len(hits) != 1 {
		t.Errorf("ListPage(source) on a healed database = (%d, %v), want one run", len(hits), err)
	}
}
