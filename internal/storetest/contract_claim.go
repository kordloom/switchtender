package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// testClaim verifies claiming takes the oldest unclaimed plain run exactly once and skips
// children, parents, and non-pending runs.
func testClaim(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	parentID := "run_split"
	unstartedID := "run_unstarted"
	idx, count := 0, 1
	for _, r := range []*run.Run{
		{ID: "run_new", Playbook: "p", Status: run.StatusPending, CreatedAt: base.Add(time.Minute)},
		{ID: "run_old", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_done", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: base},
		// The parent is running, which is what says a coordinator took it and means to run it. A
		// shard under a parent that has not reached that state is not claimable, because shards are
		// stored before the coordinator fences the parent.
		{ID: parentID, Playbook: "p", Kind: run.KindSplit, Status: run.StatusRunning, CreatedAt: base},
		{
			ID: "run_shard", Playbook: "p", Status: run.StatusPending,
			CreatedAt: base.Add(30 * time.Minute),
			ParentID:  &parentID, ShardIndex: &idx, ShardCount: &count,
		},
		{ID: "run_unstarted", Playbook: "p", Kind: run.KindSplit, Status: run.StatusPending,
			CreatedAt: base},
		{
			ID: "run_unstarted_c0", Playbook: "p", Status: run.StatusPending,
			CreatedAt: base.Add(time.Minute), ParentID: &unstartedID,
			ShardIndex: &idx, ShardCount: &count,
		},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	first, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if first.ID != "run_old" || first.ClaimedBy != "worker-a" || first.ClaimedAt == nil {
		t.Errorf("first claim = %+v, want run_old leased by worker-a", first)
	}

	second, err := store.Claim(ctx, "worker-b", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if second.ID != "run_new" {
		t.Errorf("second claim = %s, want run_new", second.ID)
	}

	third, err := store.Claim(ctx, "worker-c", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if third.ID != "run_shard" {
		t.Errorf("third claim = %s, want run_shard, a child of a running parent is executable",
			third.ID)
	}

	// The fourth claim finds nothing: the only run left is a shard whose parent has not started, and
	// claiming that is what let a split canceled before its coordinator ran execute anyway.
	if got, err := store.Claim(ctx, "worker-d", []string{""}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("fourth claim = (%v, %v), want ErrNonePending: a shard whose parent has not "+
			"started is claimable, so a split canceled in that window still runs", got, err)
	}
}

// testClaimQueue verifies a queued run is only claimable by an executor serving that queue.
func testClaimQueue(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "run_default", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_dmz", Playbook: "p", Status: run.StatusPending, CreatedAt: base, Queue: "dmz"},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	// A default executor claims the default run and never the dmz run.
	got, err := store.Claim(ctx, "serve", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if got.ID != "run_default" {
		t.Errorf("default executor claimed %s, want run_default", got.ID)
	}
	if _, err := store.Claim(ctx, "serve", []string{""}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("default executor second claim = %v, want ErrNonePending, the dmz run is off limits", err)
	}

	// A dmz worker claims the dmz run.
	dmz, err := store.Claim(ctx, "dmz-worker", []string{"dmz"})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if dmz.ID != "run_dmz" {
		t.Errorf("dmz worker claimed %s, want run_dmz", dmz.ID)
	}
}

// testLeaseLifecycle verifies heartbeats renew a lease and stale leases requeue pending runs and
// interrupt running ones.
func testLeaseLifecycle(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_q", Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	claimed, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	if err := store.Heartbeat(ctx, claimed.ID, "worker-a"); err != nil {
		t.Errorf("Heartbeat() error = %v", err)
	}
	if err := store.Heartbeat(ctx, claimed.ID, "impostor"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Heartbeat() wrong owner error = %v, want ErrNotFound", err)
	}

	// A fresh lease survives a sweep that only reclaims leases older than a minute.
	n, err := store.ReclaimStale(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	if n != 0 {
		t.Errorf("ReclaimStale() = %d, want 0 while the lease is fresh", n)
	}

	// A zero age makes every held lease stale: the pending run goes back in the queue.
	n, err = store.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	if n != 1 {
		t.Errorf("ReclaimStale() = %d, want 1 requeued", n)
	}
	back, err := store.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if back.ClaimedBy != "" || back.Status != run.StatusPending {
		t.Errorf("requeued run = %+v, want unclaimed pending", back)
	}

	// A stale lease on a running run interrupts it instead.
	reclaimed, err := store.Claim(ctx, "worker-b", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	reclaimed.Status = run.StatusRunning
	if err := store.Save(ctx, reclaimed); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if n, err = store.ReclaimStale(ctx, 0); err != nil || n != 1 {
		t.Fatalf("ReclaimStale() = %d, %v, want 1 interrupted", n, err)
	}
	gone, err := store.Get(ctx, reclaimed.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if gone.Status != run.StatusInterrupted || gone.EndedAt == nil {
		t.Errorf("stale running run = %+v, want interrupted with an end time", gone)
	}
	// The interrupt also releases the lease so the reclaimed worker's heartbeat stops matching and
	// the run cannot be resurrected through its stale owner.
	if gone.ClaimedBy != "" || gone.ClaimedAt != nil {
		t.Errorf("interrupted run still leased = %+v, want claimed_by and claimed_at cleared", gone)
	}
	if err := store.Heartbeat(ctx, gone.ID, "worker-b"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Heartbeat() on interrupted run = %v, want ErrNotFound", err)
	}
}

// testClaimSecret verifies a claim mints a fresh per-claim capability, that it is stored and read
// back, that two claims never share one, and that a reclaim clears it so a report minted against the
// lost claim no longer verifies against the run. This is the capability that authorizes a worker's
// relay reports, so it must be present, unique, persisted, and revoked on reclaim.
func testClaimSecret(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_secret", Playbook: "p", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	first, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if first.ClaimSecret == "" {
		t.Fatal("Claim() returned an empty claim secret, want a fresh capability")
	}

	// The secret is stored, not only returned, so the control node can verify a later report against
	// it. Get reads the same column Save wrote.
	stored, err := store.Get(ctx, "run_secret")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.ClaimSecret != first.ClaimSecret {
		t.Errorf("stored secret = %q, want the one the claim returned %q",
			stored.ClaimSecret, first.ClaimSecret)
	}

	// A reclaim releases the run and, with it, the capability: the report a worker could still mint
	// from the first claim no longer matches the run.
	if _, err := store.ReclaimStale(ctx, 0); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	back, err := store.Get(ctx, "run_secret")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if back.ClaimSecret != "" {
		t.Errorf("reclaimed run still carries a claim secret %q, want it cleared", back.ClaimSecret)
	}

	// The re-claim mints a new capability. It must not equal the first, or a stale report would
	// still verify after the run changed hands.
	second, err := store.Claim(ctx, "worker-b", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if second.ClaimSecret == "" || second.ClaimSecret == first.ClaimSecret {
		t.Errorf("re-claim secret = %q, want a fresh value distinct from the first %q",
			second.ClaimSecret, first.ClaimSecret)
	}
}

// testReclaimOrphans verifies a dead coordinator does not leave its children stranded. When a stale
// split or pipeline parent is interrupted, nothing is left to roll its children up, so a child no
// executor started must not stay pending and claimable, and one already executing must be told to
// stop. Without this a killed coordinator's shards run on with no parent to report them.
func testReclaimOrphans(t *testing.T, store run.Store) {
	ctx := context.Background()
	stale := time.Now().Add(-time.Hour)
	fresh := time.Now()
	parentID := "run_orphan_parent"
	parent := &run.Run{
		ID: parentID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusRunning,
		ClaimedBy: "dead-coordinator", ClaimedAt: &stale, CreatedAt: stale,
	}
	idx0, idx1, count := 0, 1, 2
	queued := &run.Run{
		ID: "run_orphan_queued", Playbook: "site.yml", Status: run.StatusPending,
		ParentID: &parentID, ShardIndex: &idx0, ShardCount: &count, CreatedAt: stale,
	}
	executing := &run.Run{
		ID: "run_orphan_running", Playbook: "site.yml", Status: run.StatusRunning,
		ParentID: &parentID, ShardIndex: &idx1, ShardCount: &count, CreatedAt: stale,
		ClaimedBy: "live-worker", ClaimedAt: &fresh,
	}
	for _, r := range []*run.Run{parent, queued, executing} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	// Only the parent's lease is stale, so the executing child is swept as an orphan rather than
	// as a lease that expired on its own.
	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	gotParent, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get(parent) error = %v", err)
	}
	if gotParent.Status != run.StatusInterrupted {
		t.Fatalf("parent status = %q, want interrupted", gotParent.Status)
	}
	gotQueued, err := store.Get(ctx, queued.ID)
	if err != nil {
		t.Fatalf("Get(queued) error = %v", err)
	}
	if gotQueued.Status != run.StatusCanceled {
		t.Errorf("queued child status = %q, want canceled, not left claimable", gotQueued.Status)
	}
	if gotQueued.EndedAt == nil {
		t.Error("queued child has no end time, so it never finished")
	}
	if gotQueued.Error != run.OrphanError() {
		t.Errorf("queued child error = %q, want %q", gotQueued.Error, run.OrphanError())
	}
	gotRunning, err := store.Get(ctx, executing.ID)
	if err != nil {
		t.Fatalf("Get(executing) error = %v", err)
	}
	if !gotRunning.CancelRequested {
		t.Error("executing child was not asked to stop, so it runs on with no parent to report it")
	}

	// The queued child is no longer work: a claim must not hand it out.
	if claimed, err := store.Claim(ctx, "worker-x", []string{""}); err == nil {
		t.Errorf("Claim() returned %s, want nothing claimable after the sweep", claimed.ID)
	} else if !errors.Is(err, run.ErrNonePending) {
		t.Errorf("Claim() error = %v, want ErrNonePending", err)
	}
}

// testRequestCancel verifies the cancel flag round trips and unknown runs report ErrNotFound.
func testRequestCancel(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_c", Playbook: "p", Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.RequestCancel(ctx, "run_c"); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}
	got, err := store.Get(ctx, "run_c")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CancelRequested {
		t.Error("CancelRequested not set after RequestCancel")
	}
	if err := store.RequestCancel(ctx, "ghost"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("RequestCancel(ghost) error = %v, want ErrNotFound", err)
	}
}

// testCancelPending verifies the unclaimed-run cancel: it terminalizes a waiting pending or
// pending_approval run, refuses a claimed, executing, terminal, or missing run, and stamps the
// end time.
func testCancelPending(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	claimed := base.Add(time.Minute)
	for _, r := range []*run.Run{
		{ID: "run_wait", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_held", Playbook: "p", Status: run.StatusPendingApproval, CreatedAt: base},
		{ID: "run_taken", Playbook: "p", Status: run.StatusPending, CreatedAt: base, ClaimedBy: "w1", ClaimedAt: &claimed},
		{ID: "run_live", Playbook: "p", Status: run.StatusRunning, CreatedAt: base, ClaimedBy: "w1", ClaimedAt: &claimed},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	ok, err := store.CancelPending(ctx, "run_wait")
	if err != nil || !ok {
		t.Fatalf("CancelPending(run_wait) = (%v, %v), want (true, nil)", ok, err)
	}
	got, err := store.Get(ctx, "run_wait")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusCanceled || got.EndedAt == nil {
		t.Errorf("canceled run = %q ended %v, want canceled with an end time", got.Status, got.EndedAt)
	}
	if ok, err := store.CancelPending(ctx, "run_wait"); err != nil || ok {
		t.Errorf("second CancelPending = (%v, %v), want (false, nil)", ok, err)
	}

	if ok, err := store.CancelPending(ctx, "run_held"); err != nil || !ok {
		t.Errorf("CancelPending(run_held) = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := store.CancelPending(ctx, "run_taken"); err != nil || ok {
		t.Errorf("CancelPending(run_taken) = (%v, %v), want (false, nil)", ok, err)
	}
	if r, _ := store.Get(ctx, "run_taken"); r.Status != run.StatusPending {
		t.Errorf("claimed run status = %q, want pending untouched", r.Status)
	}
	if ok, err := store.CancelPending(ctx, "run_live"); err != nil || ok {
		t.Errorf("CancelPending(run_live) = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := store.CancelPending(ctx, "ghost"); err != nil || ok {
		t.Errorf("CancelPending(ghost) = (%v, %v), want (false, nil)", ok, err)
	}
}

// testSaveKeepsCancel verifies the sticky cancel flag: replacing a run from a snapshot taken
// before the cancel was requested must not erase the stored flag.
func testSaveKeepsCancel(t *testing.T, store run.Store) {
	ctx := context.Background()
	r := &run.Run{ID: "run_sc", Playbook: "p", Status: run.StatusPending,
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.RequestCancel(ctx, "run_sc"); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}

	stale := r.Clone()
	stale.Status = run.StatusRunning
	stale.CancelRequested = false
	if err := store.Save(ctx, stale); err != nil {
		t.Fatalf("Save(stale) error = %v", err)
	}

	got, err := store.Get(ctx, "run_sc")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CancelRequested {
		t.Error("cancel flag erased by a stale save, want it kept")
	}
	if got.Status != run.StatusRunning {
		t.Errorf("status = %q, want running from the save", got.Status)
	}
}

// testClaimSkipsCancel verifies a pending run whose cancel was requested is never claimed.
func testClaimSkipsCancel(t *testing.T, store run.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "run_stop", Playbook: "p", Status: run.StatusPending, CreatedAt: base},
		{ID: "run_go", Playbook: "p", Status: run.StatusPending, CreatedAt: base.Add(time.Minute)},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	if err := store.RequestCancel(ctx, "run_stop"); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}

	got, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if got.ID != "run_go" {
		t.Errorf("claimed %s, want run_go; the cancel-requested run must be skipped", got.ID)
	}
	if _, err := store.Claim(ctx, "worker-b", []string{""}); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("second Claim() error = %v, want ErrNonePending", err)
	}
}

// testTerminalFence verifies the store fences auxiliary writes to a terminal run: a reclaimed-but-alive
// worker's late logs, events, and summaries are dropped rather than appended or overwritten, and the
// run is not resurrected. The writes return no error, so a benign late write does not look like a
// failure.
func testTerminalFence(t *testing.T, store run.Store) {
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	// The run records its output while running.
	if err := store.Save(ctx, &run.Run{ID: "fx", Status: run.StatusRunning, CreatedAt: created}); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	if err := store.AppendLog(ctx, "fx", []byte("early")); err != nil {
		t.Fatalf("AppendLog(early) error = %v", err)
	}
	if err := store.AppendEvents(ctx, "fx",
		[]event.Event{{Type: event.TypePlayStart, Time: created, Play: "p"}}); err != nil {
		t.Fatalf("AppendEvents(early) error = %v", err)
	}

	// It finalizes.
	if err := store.Save(ctx, &run.Run{ID: "fx", Status: run.StatusSucceeded, CreatedAt: created}); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	// A reclaimed-but-alive worker's late writes are dropped, not errors.
	if err := store.AppendLog(ctx, "fx", []byte("late")); err != nil {
		t.Errorf("AppendLog(late) error = %v, want a silent no-op", err)
	}
	if err := store.AppendEvents(ctx, "fx",
		[]event.Event{{Type: event.TypePlayStart, Time: created, Play: "zombie"}}); err != nil {
		t.Errorf("AppendEvents(late) error = %v, want a silent no-op", err)
	}
	if err := store.SaveHostSummary(ctx, "fx",
		[]run.HostSummary{{Host: "zombie", Worst: "failures", RanAt: created}}); err != nil {
		t.Errorf("SaveHostSummary(late) error = %v, want a silent no-op", err)
	}
	if err := store.SaveTaskSummary(ctx, "fx",
		[]run.TaskSummary{{Task: "zombie", Seconds: 9}}); err != nil {
		t.Errorf("SaveTaskSummary(late) error = %v, want a silent no-op", err)
	}

	// The late writes landed nowhere and the run keeps its terminal state.
	if body, err := store.Log(ctx, "fx"); err != nil || string(body) != "early" {
		t.Errorf("Log = %q (err %v), want %q with the late write dropped", body, err, "early")
	}
	if evs, err := store.Events(ctx, "fx"); err != nil || len(evs) != 1 {
		t.Errorf("Events len = %d (err %v), want only the pre-terminal event", len(evs), err)
	}
	if got, err := store.Get(ctx, "fx"); err != nil || got.Status != run.StatusSucceeded {
		t.Errorf("run status = %v (err %v), want succeeded, not resurrected", got.Status, err)
	}
}

// testReclaimAbandonedParents verifies a split or pipeline parent that no coordinator ever started
// does not strand its children.
//
// A parent is saved before its children, and the coordinator that would run it starts only after
// every child is written. A child save that fails, or a process that dies in that window, leaves the
// parent pending with no lease. Nothing claims a run with a kind, so no worker will ever take it,
// and orphan resolution only fires for an interrupted parent, so the parent sat pending forever
// while its children stayed claimable and ran with nothing to roll them up.
//
// A parent awaiting approval is the case this must not touch. It is resting for as long as a person
// takes to decide, and Approve starts its coordinator, so sweeping it would cancel every gated split
// and workflow that outlived one sweep interval.
func testReclaimAbandonedParents(t *testing.T, store run.Store) {
	ctx := context.Background()
	stale := time.Now().Add(-time.Hour)
	abandonedID, heldID, freshID := "run_abandoned", "run_held", "run_fresh_parent"
	idx, count := 0, 1
	child := func(id, parent string, status run.Status) *run.Run {
		p := parent
		return &run.Run{
			ID: id, Playbook: "site.yml", Status: status, ParentID: &p,
			ShardIndex: &idx, ShardCount: &count, CreatedAt: stale,
		}
	}
	saved := []*run.Run{
		// A parent whose coordinator never started, old enough to be past any sweep cutoff.
		{ID: abandonedID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusPending,
			CreatedAt: stale},
		child("run_abandoned_shard", abandonedID, run.StatusPending),
		// A parent held for an approver, equally old. It is waiting on a person, not abandoned.
		{ID: heldID, Playbook: "site.yml", Kind: run.KindSplit,
			Status: run.StatusPendingApproval, CreatedAt: stale},
		child("run_held_shard", heldID, run.StatusPendingApproval),
		// A parent submitted just now, whose coordinator is about to save it running.
		{ID: freshID, Playbook: "site.yml", Kind: run.KindPipeline, Status: run.StatusPending,
			CreatedAt: time.Now()},
	}
	for _, r := range saved {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	gotAbandoned, err := store.Get(ctx, abandonedID)
	if err != nil {
		t.Fatalf("Get(abandoned) error = %v", err)
	}
	if gotAbandoned.Status != run.StatusInterrupted {
		t.Errorf("abandoned parent status = %q, want interrupted: nothing claims a run with a "+
			"kind, so it waits forever while its children stay claimable", gotAbandoned.Status)
	}
	if gotAbandoned.EndedAt == nil {
		t.Error("abandoned parent has no end time, so it never finished")
	}
	if gotAbandoned.Error != run.AbandonedParentError() {
		t.Errorf("abandoned parent error = %q, want %q", gotAbandoned.Error,
			run.AbandonedParentError())
	}
	// Interrupting the parent is only worth doing because it settles the children in the same sweep.
	gotShard, err := store.Get(ctx, "run_abandoned_shard")
	if err != nil {
		t.Fatalf("Get(abandoned shard) error = %v", err)
	}
	if gotShard.Status != run.StatusCanceled {
		t.Errorf("shard of an abandoned parent status = %q, want canceled: a pending shard is "+
			"claimable and runs with nothing to roll it up", gotShard.Status)
	}

	gotHeld, err := store.Get(ctx, heldID)
	if err != nil {
		t.Fatalf("Get(held) error = %v", err)
	}
	if gotHeld.Status != run.StatusPendingApproval {
		t.Errorf("held parent status = %q, want pending_approval: a run awaiting a person was "+
			"canceled for taking longer than one sweep interval", gotHeld.Status)
	}
	gotHeldShard, err := store.Get(ctx, "run_held_shard")
	if err != nil {
		t.Fatalf("Get(held shard) error = %v", err)
	}
	if gotHeldShard.Status != run.StatusPendingApproval {
		t.Errorf("shard of a held parent status = %q, want pending_approval", gotHeldShard.Status)
	}

	gotFresh, err := store.Get(ctx, freshID)
	if err != nil {
		t.Fatalf("Get(fresh) error = %v", err)
	}
	if gotFresh.Status != run.StatusPending {
		t.Errorf("freshly submitted parent status = %q, want pending: its coordinator was still "+
			"starting", gotFresh.Status)
	}
}

// testReclaimApprovedParentWithNoCoordinator verifies a parent released by an approval, whose
// coordinator never arrived, is still settled.
//
// An approved parent goes straight to running so the sweep cannot catch it in the instant before its
// coordinator claims it. That leaves running-and-unclaimed as the state meaning the coordinator
// never arrived, and neither the lease sweep, which only looks at leased runs, nor a pending-only
// abandoned rule covers it. Without this the fix for one race created a parent nothing would finish.
func testReclaimApprovedParentWithNoCoordinator(t *testing.T, store run.Store) {
	ctx := context.Background()
	stale := time.Now().Add(-time.Hour)
	parentID := "run_approved_no_coordinator"
	idx, count := 0, 1
	child := parentID
	saved := []*run.Run{
		// Released by an approval, then the process handling it went away.
		{ID: parentID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusRunning,
			CreatedAt: stale},
		{ID: "run_approved_shard", Playbook: "site.yml", Status: run.StatusPending,
			ParentID: &child, ShardIndex: &idx, ShardCount: &count, CreatedAt: stale},
	}
	for _, r := range saved {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	got, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("parent status = %q, want interrupted: running with no lease means no coordinator "+
			"ever claimed it, and nothing else sweeps that state", got.Status)
	}
	shard, err := store.Get(ctx, "run_approved_shard")
	if err != nil {
		t.Fatalf("Get(shard) error = %v", err)
	}
	if shard.Status != run.StatusCanceled {
		t.Errorf("shard status = %q, want canceled: it is claimable under a parent with no "+
			"coordinator", shard.Status)
	}
}

// testReclaimLeavesACoordinatedParentAlone verifies the widened rule does not touch a parent whose
// coordinator holds it, which is every healthy split and pipeline.
func testReclaimLeavesACoordinatedParentAlone(t *testing.T, store run.Store) {
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	now := time.Now()
	healthy := &run.Run{
		ID: "run_coordinated", Playbook: "site.yml", Kind: run.KindSplit,
		Status: run.StatusRunning, CreatedAt: old, ClaimedBy: "coordinator-a", ClaimedAt: &now,
	}
	if err := store.Save(ctx, healthy); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := store.ReclaimStale(ctx, 30*time.Minute); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	got, err := store.Get(ctx, "run_coordinated")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("a coordinated parent was swept: status = %q. Its lease is fresh, so it is being "+
			"actively coordinated", got.Status)
	}
}

// testClaimSkipsChildrenOfSettledParents checks that a shard is not claimable under a parent that is
// already settled or being canceled.
func testClaimSkipsChildrenOfSettledParents(t *testing.T, store run.Store) {
	ctx := context.Background()
	parent := &run.Run{
		ID: "run_dead_parent", Playbook: "site.yml", Kind: run.KindSplit,
		Status: run.StatusCanceled, CreatedAt: time.Now().Add(-time.Hour),
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save() parent error = %v", err)
	}
	idx, count := 0, 2
	child := &run.Run{
		ID: "run_dead_parent_c0", Playbook: "site.yml", Status: run.StatusPending,
		CreatedAt: time.Now().Add(-time.Hour), ParentID: &parent.ID,
		ShardIndex: &idx, ShardCount: &count,
	}
	if err := store.Save(ctx, child); err != nil {
		t.Fatalf("Save() child error = %v", err)
	}
	got, err := store.Claim(ctx, "worker-a", []string{""})
	if err == nil {
		t.Fatalf("claimed %q, a shard of a canceled split, so it executes on real hosts", got.ID)
	}
	if !errors.Is(err, run.ErrNonePending) {
		t.Fatalf("Claim() error = %v, want ErrNonePending", err)
	}
}

// testLeaseClockIsOneClock verifies that whatever stamps a lease and whatever ages it agree about
// what time it is.
//
// Claim, Heartbeat, and ReclaimStale all read a clock. If they do not read the same one, a worker
// whose clock runs behind renews its lease into the past and the next sweep interrupts a perfectly
// healthy run. Heartbeating then makes things worse than not heartbeating, because a run that never
// renews keeps the stamp Claim gave it and survives. This is checked by measuring the stamps rather
// than by reading the code, because the two happened to differ by only milliseconds on one machine
// and by a minute on another.
func testLeaseClockIsOneClock(t *testing.T, store run.Store) {
	ctx := context.Background()
	id := "run_lease_clock"
	if err := store.Save(ctx, &run.Run{
		ID: id, Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	claimed, err := store.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.ClaimedAt == nil {
		t.Fatal("Claim() stamped no lease time")
	}
	atClaim := *claimed.ClaimedAt

	if err := store.Heartbeat(ctx, id, "worker-a"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ClaimedAt == nil {
		t.Fatal("Heartbeat() left no lease time")
	}
	// A renewal must never move the lease backwards. Two clocks in one column is exactly what that
	// looks like from the outside, whichever way they are skewed.
	if got.ClaimedAt.Before(atClaim) {
		t.Errorf("a heartbeat moved the lease from %s back to %s, so whatever stamps it and "+
			"whatever ages it are reading different clocks, and the sweep will interrupt a healthy "+
			"run", atClaim.Format(time.RFC3339Nano), got.ClaimedAt.Format(time.RFC3339Nano))
	}
	// And a freshly renewed lease is not stale, which is the outcome that actually hurts.
	if _, err := store.ReclaimStale(ctx, 30*time.Second); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	after, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() after sweep error = %v", err)
	}
	if after.Status != run.StatusPending || after.ClaimedBy != "worker-a" {
		t.Errorf("a run that just heartbeated is %q held by %q after the sweep, so a live worker "+
			"was cut off", after.Status, after.ClaimedBy)
	}
}

// testRunRacesUnderConcurrency verifies the run store's compare-and-swap operations actually
// serialize, rather than appearing to because the tests above ran one call at a time.
//
// Every operation here decides whether work runs on real hosts, and several control nodes and
// workers hit one database at once. A sequential test passes on a store where none of this holds,
// which is exactly how a guard that did nothing under load shipped once already.
//
// Three properties, each measured rather than asserted:
//
//   - Only one caller may claim a given run. Two executors running the same playbook on the same
//     hosts is the most expensive failure this store can produce.
//   - A run cannot be both canceled and claimed. Whoever loses must see that they lost, because a
//     cancel that reports success while an executor starts the work is the defect this codebase has
//     produced more times than any other.
//   - Only one caller may move a run out of a given status, so a run cannot be approved twice or
//     finalized into two different outcomes.
func testRunRacesUnderConcurrency(t *testing.T, store run.Store) {
	ctx := context.Background()
	const racers = 8

	for round := range 25 {
		// One run, many claimers.
		id := fmt.Sprintf("run_race_claim_%d", round)
		if err := store.Save(ctx, &run.Run{
			ID: id, Playbook: "site.yml", Status: run.StatusPending,
			CreatedAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		var claims atomic.Int64
		race(racers, func(i int) {
			got, err := store.Claim(ctx, fmt.Sprintf("worker-%d", i), []string{""})
			switch {
			case errors.Is(err, run.ErrNonePending):
			case err != nil:
				t.Errorf("Claim() error = %v", err)
			case got != nil && got.ID == id:
				claims.Add(1)
			}
		})
		if got := claims.Load(); got != 1 {
			t.Fatalf("round %d: %d executors claimed the same run, so the same playbook runs on "+
				"the same hosts %d times", round, got, got)
		}

		// Cancel racing claim: at most one may win, and the winner must be the only one.
		cid := fmt.Sprintf("run_race_cancel_%d", round)
		if err := store.Save(ctx, &run.Run{
			ID: cid, Playbook: "site.yml", Status: run.StatusPending,
			CreatedAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", cid, err)
		}
		var canceled, claimed atomic.Int64
		race(racers, func(i int) {
			if i%2 == 0 {
				ok, err := store.CancelPending(ctx, cid)
				if err != nil {
					t.Errorf("CancelPending() error = %v", err)
					return
				}
				if ok {
					canceled.Add(1)
				}
				return
			}
			got, err := store.Claim(ctx, fmt.Sprintf("worker-%d", i), []string{""})
			if err == nil && got != nil && got.ID == cid {
				claimed.Add(1)
			}
		})
		if canceled.Load() > 1 {
			t.Fatalf("round %d: %d callers were told they canceled the same run", round, canceled.Load())
		}
		if canceled.Load() == 1 && claimed.Load() > 0 {
			t.Fatalf("round %d: a run was reported canceled and claimed for execution as well, "+
				"so work starts on hosts after the API said it would not", round)
		}

		// One status transition out of a given state.
		tid := fmt.Sprintf("run_race_transition_%d", round)
		if err := store.Save(ctx, &run.Run{
			ID: tid, Playbook: "site.yml", Kind: run.KindSplit,
			Status: run.StatusPendingApproval, CreatedAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", tid, err)
		}
		var released atomic.Int64
		race(racers, func(i int) {
			ok, err := store.TransitionStatusAndClaim(ctx, tid,
				run.StatusPendingApproval, run.StatusRunning, fmt.Sprintf("coordinator-%d", i))
			if err != nil {
				t.Errorf("TransitionStatusAndClaim() error = %v", err)
				return
			}
			if ok {
				released.Add(1)
			}
		})
		if got := released.Load(); got != 1 {
			t.Fatalf("round %d: %d approvals released the same held run, so a split fans out %d "+
				"times", round, got, got)
		}
	}
}

// testReclaimAttribution holds ReclaimStaleSettled to its one hard rule: it names exactly the
// top-level runs the sweep itself drove terminal. A run whose real finisher already recorded its end
// must not be attributed to the sweep, or the janitor commits a second, contradictory outcome entry
// for it and the run's receipt permanently fails verification. A child is never attributed, since its
// outcome rolls up into its parent.
func testReclaimAttribution(t *testing.T, store run.Store) {
	reporter, ok := store.(settledReporter)
	if !ok {
		t.Fatalf("%T does not implement ReclaimStaleSettled", store)
	}
	ctx := context.Background()
	stale := time.Now().Add(-2 * time.Hour)

	// A genuinely stale top-level run: its worker died mid-change, so the sweep settles it.
	dead := &run.Run{ID: "r_dead", Playbook: "p", Status: run.StatusRunning, CreatedAt: stale,
		ClaimedBy: "dead-worker", ClaimedAt: &stale, StartedAt: &stale}
	// A run whose executor finished it properly before the sweep ran: terminal, not the sweep's.
	code := 0
	endedAt := time.Now()
	finished := &run.Run{ID: "r_finished", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: stale,
		ClaimedBy: "live-worker", ClaimedAt: &stale, StartedAt: &stale, ExitCode: &code, EndedAt: &endedAt}
	// A terminal split parent whose stale child the sweep settles: the child is never attributed.
	parent := &run.Run{ID: "r_parent", Playbook: "p", Status: run.StatusSucceeded, Kind: run.KindSplit,
		CreatedAt: stale, EndedAt: &endedAt}
	child := &run.Run{ID: "r_child", Playbook: "p", Status: run.StatusRunning, CreatedAt: stale,
		ClaimedBy: "dead-worker", ClaimedAt: &stale, StartedAt: &stale, ParentID: &parent.ID}
	for _, r := range []*run.Run{dead, finished, parent, child} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}

	n, settled, err := reporter.ReclaimStaleSettled(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStaleSettled: %v", err)
	}
	if n == 0 {
		t.Fatal("the sweep changed nothing, want the stale runs settled")
	}
	if diff := cmp.Diff([]string{"r_dead"}, settled); diff != "" {
		t.Errorf("settled attribution mismatch (-want +got):\n%s", diff)
	}

	// The already-finished run is untouched, and both stale runs were interrupted.
	got, err := store.Get(ctx, "r_finished")
	if err != nil || got.Status != run.StatusSucceeded {
		t.Errorf("finished run = %v/%v, want untouched succeeded", got.Status, err)
	}
	for _, id := range []string{"r_dead", "r_child"} {
		got, err := store.Get(ctx, id)
		if err != nil || got.Status != run.StatusInterrupted {
			t.Errorf("%s = %v/%v, want interrupted by the sweep", id, got.Status, err)
		}
	}
}
