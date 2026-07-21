package pgstore_test

import (
	"context"
	"database/sql"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/dispatch"
	"github.com/dcadolph/switchtender/internal/pgstore"
	"github.com/dcadolph/switchtender/internal/roundhouse"
	"github.com/dcadolph/switchtender/internal/run"
	"github.com/dcadolph/switchtender/internal/schedule"
)

// openReplica opens an independent connection to the shared test database, standing in for one
// server replica.
func openReplica(t *testing.T, dsn string) *pgstore.DB {
	t.Helper()
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// waitFor polls until check passes or the deadline hits, failing the test on timeout.
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHAReplicasShareTheQueue proves two active replicas divide pending work without ever claiming
// the same run twice: every run finishes, every claim names one replica, and both replicas do work.
func TestHAReplicasShareTheQueue(t *testing.T) {
	dsn := testDSN(t)
	// Opening applies the schema, so a fresh database has tables before the truncate.
	openReplica(t, dsn)
	truncateAll(t, dsn)

	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			time.Sleep(30 * time.Millisecond)
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	a := dispatch.New(openReplica(t, dsn).Runs(), runner, zap.NewNop(),
		dispatch.WithOwner("replica-a"), dispatch.WithClaimInterval(20*time.Millisecond))
	defer a.Close()
	b := dispatch.New(openReplica(t, dsn).Runs(), runner, zap.NewNop(),
		dispatch.WithOwner("replica-b"), dispatch.WithClaimInterval(20*time.Millisecond))
	defer b.Close()

	store := openReplica(t, dsn).Runs()
	const total = 20
	ids := make([]string, 0, total)
	for range total {
		r, err := a.Submit(context.Background(), "play.yml", "inv.ini")
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		ids = append(ids, r.ID)
	}

	owners := map[string]int{}
	waitFor(t, "all runs to finish", func() bool {
		owners = map[string]int{}
		for _, id := range ids {
			r, err := store.Get(context.Background(), id)
			if err != nil || !r.Status.Terminal() {
				return false
			}
			owners[r.ClaimedBy]++
		}
		return true
	})

	for _, id := range ids {
		r, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if r.Status != run.StatusSucceeded {
			t.Errorf("run %s status = %s, want succeeded", id, r.Status)
		}
		if r.ClaimedBy != "replica-a" && r.ClaimedBy != "replica-b" {
			t.Errorf("run %s claimed by %q, want replica-a or replica-b", id, r.ClaimedBy)
		}
	}
	if owners["replica-a"] == 0 || owners["replica-b"] == 0 {
		t.Errorf("claim distribution = %v, want both replicas participating", owners)
	}
}

// TestHAIdempotentSubmitNeverDoubleFires proves the store-backed dedup holds across replicas: many
// submits split over two active replicas, all carrying one idempotency key, create a single run.
// This is the case a client retry after a dropped response causes, landing on a different node than
// the original, which an in-memory guard on one node could not catch.
func TestHAIdempotentSubmitNeverDoubleFires(t *testing.T) {
	dsn := testDSN(t)
	// Opening applies the schema, so a fresh database has tables before the truncate.
	openReplica(t, dsn)
	truncateAll(t, dsn)

	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	a := dispatch.New(openReplica(t, dsn).Runs(), runner, zap.NewNop(),
		dispatch.WithOwner("replica-a"), dispatch.WithClaimInterval(20*time.Millisecond))
	defer a.Close()
	b := dispatch.New(openReplica(t, dsn).Runs(), runner, zap.NewNop(),
		dispatch.WithOwner("replica-b"), dispatch.WithClaimInterval(20*time.Millisecond))
	defer b.Close()

	const key = "idem-ha"
	const total = 10
	replicas := []*dispatch.Dispatcher{a, b}
	ids := make([]string, total)
	errs := make([]error, total)
	var wg sync.WaitGroup
	wg.Add(total)
	for i := range total {
		go func(i int) {
			defer wg.Done()
			r, err := replicas[i%2].Submit(context.Background(), "play.yml", "inv.ini",
				run.WithIdempotencyKey(key))
			errs[i] = err
			if r != nil {
				ids[i] = r.ID
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Submit[%d] error = %v", i, err)
		}
	}
	for i := 1; i < total; i++ {
		if ids[i] != ids[0] {
			t.Errorf("submit %d id = %q, want %q, every replica must resolve the key to one run", i, ids[i], ids[0])
		}
	}
	store := openReplica(t, dsn).Runs()
	runs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("List() len = %d, want exactly 1 run despite %d submits across two replicas", len(runs), total)
	}
}

// countingSubmitter counts fires so the double-fire test needs no execution machinery.
type countingSubmitter struct {
	// fired counts Submit calls across replicas.
	fired atomic.Int64
}

// Submit records the fire and returns a synthetic run.
func (c *countingSubmitter) Submit(_ context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error) {
	c.fired.Add(1)
	r := &run.Run{ID: run.NewID(), Playbook: playbook, Inventory: inventory, Status: run.StatusPending}
	run.ApplyOptions(r, opts)
	return r, nil
}

// SubmitSplit records the fire like Submit.
func (c *countingSubmitter) SubmitSplit(ctx context.Context, playbook, inventory string, _ int, opts ...run.SubmitOption) (*run.Run, error) {
	return c.Submit(ctx, playbook, inventory, opts...)
}

// SubmitPipeline records the fire like Submit.
func (c *countingSubmitter) SubmitPipeline(ctx context.Context, name, inventory string, _ []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	return c.Submit(ctx, name, inventory, opts...)
}

// TestHASchedulersNeverDoubleFire proves two replicas ticking the same due schedule fire it exactly
// once: the store's compare-and-set claim decides a single winner.
func TestHASchedulersNeverDoubleFire(t *testing.T) {
	dsn := testDSN(t)
	// Opening applies the schema, so a fresh database has tables before the truncate.
	openReplica(t, dsn)
	truncateAll(t, dsn)

	sched := openReplica(t, dsn)
	due := time.Now().Add(-time.Minute)
	if err := sched.Schedules().Save(context.Background(), &schedule.Schedule{
		ID: "sched_ha", Name: "nightly", Cron: "0 0 * * *", Playbook: "play.yml",
		Enabled: true, CreatedAt: time.Now(), NextRunAt: &due,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	counter := &countingSubmitter{}
	a := schedule.NewScheduler(openReplica(t, dsn).Schedules(), counter, zap.NewNop(),
		schedule.WithInterval(20*time.Millisecond))
	b := schedule.NewScheduler(openReplica(t, dsn).Schedules(), counter, zap.NewNop(),
		schedule.WithInterval(20*time.Millisecond))
	a.Start()
	b.Start()
	defer a.Close()
	defer b.Close()

	waitFor(t, "the schedule to fire", func() bool { return counter.fired.Load() >= 1 })
	// Hold the window open long enough for a losing replica to double-fire if the claim leaked.
	time.Sleep(500 * time.Millisecond)
	if got := counter.fired.Load(); got != 1 {
		t.Errorf("fires = %d, want exactly 1", got)
	}
}

// TestHAFailoverReclaimsStaleWork proves a run leased by a dead replica is swept back to pending
// and finished by a surviving replica.
func TestHAFailoverReclaimsStaleWork(t *testing.T) {
	dsn := testDSN(t)
	// Opening applies the schema, so a fresh database has tables before the truncate.
	openReplica(t, dsn)
	truncateAll(t, dsn)

	store := openReplica(t, dsn).Runs()
	stale := time.Now().Add(-10 * time.Minute)
	dead := &run.Run{
		ID: run.NewID(), Playbook: "play.yml", Inventory: "inv.ini",
		Status: run.StatusPending, CreatedAt: time.Now(),
	}
	if err := store.Save(context.Background(), dead); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// Lease it as a replica that then vanished.
	claimed, err := store.Claim(context.Background(), "replica-dead", nil)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.ID != dead.ID {
		t.Fatalf("Claim() = %s, want %s", claimed.ID, dead.ID)
	}
	forceStaleLease(t, dsn, dead.ID, stale)

	// A surviving replica's janitor sweeps the stale lease and its claim loop finishes the run.
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	survivor := dispatch.New(openReplica(t, dsn).Runs(), runner, zap.NewNop(),
		dispatch.WithOwner("replica-b"), dispatch.WithClaimInterval(20*time.Millisecond))
	defer survivor.Close()

	waitFor(t, "the orphaned run to finish on the survivor", func() bool {
		r, err := store.Get(context.Background(), dead.ID)
		return err == nil && r.Status == run.StatusSucceeded && r.ClaimedBy == "replica-b"
	})
}

// forceStaleLease backdates a run's lease so the janitor treats its holder as dead.
func forceStaleLease(t *testing.T, dsn, id string, at time.Time) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(),
		"UPDATE runs SET claimed_at=$1 WHERE id=$2", at.UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("backdate lease: %v", err)
	}
}
