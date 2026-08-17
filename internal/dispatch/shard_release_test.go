package dispatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// stubbornStore fails the first n attempts to release a shard from approval, standing in for the SQLite
// writer being held past its busy timeout or a Postgres failover dropping a statement.
type stubbornStore struct {
	run.Store
	// failures is how many release attempts still fail.
	failures atomic.Int64
	// attempts counts the release attempts made.
	attempts atomic.Int64
}

// TransitionStatus refuses a release while the outage lasts.
func (s *stubbornStore) TransitionStatus(ctx context.Context, id string, from, to run.Status) (bool, error) {
	if from == run.StatusPendingApproval && to == run.StatusPending {
		s.attempts.Add(1)
		if s.failures.Add(-1) >= 0 {
			return false, errors.New("database is locked (5) (SQLITE_BUSY)")
		}
	}
	return s.Store.TransitionStatus(ctx, id, from, to)
}

// TestAShardThatFailsToReleaseDoesNotStrandTheSplit covers a state a person had to notice and clean up.
//
// Approving a held split releases each shard from approval so the claim loop can take it. That write was
// a single unretried attempt whose failure was logged and stepped over, on a path whose own comment says
// contention is expected rather than exceptional and where every other write retries. A shard left in
// pending_approval is unclaimable, the coordinator waits on its children with no timeout, and the parent
// is leased and heartbeating so no sweep touches it. So one lost statement out of fifty left the other
// forty-nine running to completion and the split running forever, with nothing but a log line to say
// why, until somebody cancelled the parent by hand.
//
// The release retries now, and a shard that still cannot be released is settled with a stated reason, so
// the split reaches an end and the record says what happened to that shard.
func TestAShardThatFailsToReleaseDoesNotStrandTheSplit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	// More failures than one, fewer than the retry budget, which is the transient outage this is for.
	store := &stubbornStore{Store: base}
	store.failures.Store(2)

	shards := 3
	parent := &run.Run{
		ID: "run_split", Playbook: "site.yml", Inventory: "prod", Kind: run.KindSplit,
		Status: run.StatusPendingApproval, CreatedAt: time.Now(), ShardCount: &shards,
		Actor: "casey", ActorType: "session",
	}
	if err := base.Save(ctx, parent); err != nil {
		t.Fatalf("Save() parent error = %v", err)
	}
	for i := range shards {
		idx := i
		if err := base.Save(ctx, &run.Run{
			ID: "run_shard_" + string(rune('a'+i)), Playbook: "site.yml", Inventory: "prod",
			Status: run.StatusPendingApproval, CreatedAt: time.Now(),
			ParentID: &parent.ID, ShardIndex: &idx, ShardCount: &shards,
		}); err != nil {
			t.Fatalf("Save() shard error = %v", err)
		}
	}

	d := New(store, okRunner(), zap.NewNop(), WithNoJanitor())
	defer d.Close()
	d.startSplit(ctx, parent.Clone())

	// Every shard has to reach a terminal state, and so has the parent. A shard still held for approval
	// is the stranding: nothing can claim it and nothing settles it.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		for i := range shards {
			got, err := base.Get(ctx, "run_shard_"+string(rune('a'+i)))
			if err != nil {
				t.Fatalf("Get() shard error = %v", err)
			}
			if !got.Status.Terminal() {
				done = false
			}
		}
		gotParent, err := base.Get(ctx, parent.ID)
		if err != nil {
			t.Fatalf("Get() parent error = %v", err)
		}
		if done && gotParent.Status.Terminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	for i := range shards {
		id := "run_shard_" + string(rune('a'+i))
		got, err := base.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get() shard error = %v", err)
		}
		if got.Status == run.StatusPendingApproval {
			t.Errorf("shard %s is still held for approval, so nothing can claim it and nothing settles "+
				"it: the split runs forever until a person cancels the parent", id)
		}
		// Succeeded, not merely terminal. The outage here is transient, so every shard's release has to
		// come good and every shard has to run. Settling a shard the moment one statement failed would
		// end the split without doing the work, which is a different way to be wrong.
		if got.Status != run.StatusSucceeded {
			t.Errorf("shard %s is %q, want succeeded: a brief store outage cost this shard its "+
				"execution instead of being retried (%s)", id, got.Status, got.Error)
		}
	}
	gotParent, err := base.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() parent error = %v", err)
	}
	if !gotParent.Status.Terminal() {
		t.Errorf("the split is %q, want a terminal state", gotParent.Status)
	}
	if store.attempts.Load() < 2 {
		t.Errorf("the release was attempted %d times, so the retry never happened",
			store.attempts.Load())
	}
}

// TestAShardThatCanNeverBeReleasedIsSettled is the harder half. A retry covers a blip; an outage that
// outlasts the whole budget must still not leave the split waiting on a shard nothing can ever claim.
// The shard is recorded failed with the reason, so the split ends and the record says what happened to
// it rather than a log line saying it somewhere nobody will look.
func TestAShardThatCanNeverBeReleasedIsSettled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	store := &stubbornStore{Store: base}
	// Far past any retry budget: this release never succeeds.
	store.failures.Store(1_000_000)

	shards := 2
	parent := &run.Run{
		ID: "run_split_stuck", Playbook: "site.yml", Inventory: "prod", Kind: run.KindSplit,
		Status: run.StatusPendingApproval, CreatedAt: time.Now(), ShardCount: &shards,
		Actor: "casey", ActorType: "session",
	}
	if err := base.Save(ctx, parent); err != nil {
		t.Fatalf("Save() parent error = %v", err)
	}
	for i := range shards {
		idx := i
		if err := base.Save(ctx, &run.Run{
			ID: "run_stuck_" + string(rune('a'+i)), Playbook: "site.yml", Inventory: "prod",
			Status: run.StatusPendingApproval, CreatedAt: time.Now(),
			ParentID: &parent.ID, ShardIndex: &idx, ShardCount: &shards,
		}); err != nil {
			t.Fatalf("Save() shard error = %v", err)
		}
	}

	d := New(store, okRunner(), zap.NewNop(), WithNoJanitor())
	defer d.Close()
	d.startSplit(ctx, parent.Clone())

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, err := base.Get(ctx, parent.ID)
		if err != nil {
			t.Fatalf("Get() parent error = %v", err)
		}
		if got.Status.Terminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	gotParent, err := base.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() parent error = %v", err)
	}
	if !gotParent.Status.Terminal() {
		t.Fatalf("the split is %q: a shard that can never be released left it waiting forever",
			gotParent.Status)
	}
	for i := range shards {
		id := "run_stuck_" + string(rune('a'+i))
		got, err := base.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get() shard error = %v", err)
		}
		if got.Status != run.StatusFailed {
			t.Errorf("shard %s is %q, want failed", id, got.Status)
		}
		if got.Error == "" {
			t.Errorf("shard %s records no reason, so nobody reading the split can tell what happened "+
				"to it", id)
		}
	}
}
