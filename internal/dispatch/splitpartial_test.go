package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// errShardWrite is the write failure a shardSaveFailStore serves.
var errShardWrite = errors.New("database is locked")

// shardSaveFailStore fails the creation of the shard at one index, standing in for a database that
// stops accepting writes partway through a fan-out. Every other write passes through, so the
// settling that follows a failure is exercised against a store that works again.
type shardSaveFailStore struct {
	run.Store
	// failAt is the index of the shard whose creation fails.
	failAt int
}

// Save fails the configured shard's creation and passes everything else through.
func (s *shardSaveFailStore) Save(ctx context.Context, r *run.Run) error {
	if r.ParentID != nil && r.ShardIndex != nil && *r.ShardIndex == s.failAt {
		if _, err := s.Get(ctx, r.ID); errors.Is(err, run.ErrNotFound) {
			return errShardWrite
		}
	}
	return s.Store.Save(ctx, r)
}

// seedFailedSplit stores a finished split whose shards all failed, the shape a shard retry takes.
// stored is how many of its count shards exist, fewer than count for a split that never stored
// them all.
func seedFailedSplit(t *testing.T, store run.Store, count, stored int) string {
	t.Helper()
	ctx := context.Background()
	parentID := "run_split"
	parent := &run.Run{
		ID: parentID, Playbook: "site.yml", Inventory: "inv", Kind: run.KindSplit,
		Status: run.StatusFailed, CreatedAt: time.Now(), ShardCount: &count,
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save(parent) error = %v", err)
	}
	for i := range stored {
		idx, n := i, count
		if err := store.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_shard_%d", i), Playbook: "site.yml", Inventory: "inv",
			Status: run.StatusFailed, CreatedAt: time.Now(), ParentID: &parentID,
			ShardIndex: &idx, ShardCount: &n, Limit: fmt.Sprintf("web%02d", i),
		}); err != nil {
			t.Fatalf("Save(shard) error = %v", err)
		}
	}
	return parentID
}

// TestASplitThatCannotStoreAllItsShardsRunsNoneOfThem pins what a fan-out does when the store stops
// accepting writes partway through it. The parent was stored first and the loop returned on the
// first shard it could not store, which left a parent with only some of its shards. Held, it could
// be approved and run a subset of the hosts an approver released, and a retry of the request under
// the same key resolved to it as though it had been accepted. Unheld, it sat pending until the
// janitor interrupted it, and a shard retry of that ran only the groups that happened to be stored.
// The parent now ends failed with the reason, and every shard it stored ends canceled.
func TestASplitThatCannotStoreAllItsShardsRunsNoneOfThem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which fan-out fails and whether a rule holds it.
		Name string
		// Held reports whether a rule holds every Ansible run.
		Held bool
		// Retry reports whether the fan-out is a shard retry rather than a new split.
		Retry bool
	}{{ // Test 0: A new split a rule holds.
		Name: "held split", Held: true,
	}, { // Test 1: A new split nothing holds.
		Name: "split",
	}, { // Test 2: A shard retry a rule holds.
		Name: "held retry", Held: true, Retry: true,
	}, { // Test 3: A shard retry nothing holds.
		Name: "retry", Retry: true,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			hookURL, events := captureEvents(t)
			base := run.NewMemStore()
			runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03"}}
			opts := []Option{WithNoJanitor(), WithWebhooks([]string{hookURL}),
				WithNotifyClient(http.DefaultClient)}
			if test.Held {
				opts = append(opts, WithPolicies(ansibleWidePolicy(t)))
			}
			var parentID string
			if test.Retry {
				parentID = seedFailedSplit(t, base, 3, 3)
			}
			// The second shard's creation fails, so one shard is stored before the failure.
			store := &shardSaveFailStore{Store: base, failAt: 1}
			d := New(store, runner, nil, opts...)
			defer d.Close()

			var got *run.Run
			var err error
			if test.Retry {
				got, err = d.RetryFailedShards(ctx, parentID)
			} else {
				got, err = d.SubmitSplit(ctx, "site.yml", "inv", 3)
			}
			if !errors.Is(err, errShardWrite) {
				t.Fatalf("error = %v, want the write failure", err)
			}
			if got != nil {
				t.Errorf("a fan-out that could not be stored returned run %s", got.ID)
			}

			all, err := base.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var parent *run.Run
			for _, r := range all {
				if r.Kind == run.KindSplit && r.ID != parentID {
					parent = r
				}
			}
			if parent == nil {
				t.Fatal("no parent was stored, so there is nothing to show the failure on")
			}
			if parent.Status != run.StatusFailed ||
				!strings.Contains(parent.Error, "only 1 of 3 shards could be stored") {
				t.Errorf("parent = %s %q, want failed naming the shards it could not store",
					parent.Status, parent.Error)
			}
			shards, err := base.Shards(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Shards() error = %v", err)
			}
			for _, s := range shards {
				if s.Status != run.StatusCanceled {
					t.Errorf("shard %s is %s, want canceled: a shard of a failed fan-out must not "+
						"wait for an approval or a coordinator", s.ID, s.Status)
				}
			}
			e := nextEvent(t, events)
			if e.Event != "run.finished" || e.Run.ID != parent.ID || e.Run.Status != run.StatusFailed {
				t.Errorf("webhook event = %+v, want run.finished failed for %s", e, parent.ID)
			}
			noneArrive(t, events, "a second announcement of the failed fan-out")
			if n := runner.executions.Load(); n != 0 {
				t.Errorf("the runner ran %d times, want none", n)
			}
		})
	}
}

// TestASplitMissingShardsIsNeitherStartedNorRetried pins the backstop for a store that failed too
// hard for the fan-out to be settled, leaving a parent with fewer shards than it counts. Approving
// it would run a subset of the hosts it was approved for, and retrying it cannot know which hosts
// never had a shard, so both refuse and nothing executes.
func TestASplitMissingShardsIsNeitherStartedNorRetried(t *testing.T) {
	t.Parallel()

	t.Run("approve", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		store := run.NewMemStore()
		runner := &countingRunnerLister{}
		d := New(store, runner, nil, WithNoJanitor())
		defer d.Close()
		count, parentID := 3, "run_held_split"
		if err := store.Save(ctx, &run.Run{
			ID: parentID, Playbook: "site.yml", Kind: run.KindSplit,
			Status: run.StatusPendingApproval, CreatedAt: time.Now(), ShardCount: &count,
			HeldByPolicy: "all-ansible",
		}); err != nil {
			t.Fatalf("Save(parent) error = %v", err)
		}
		for i := range 2 {
			idx, n := i, count
			if err := store.Save(ctx, &run.Run{
				ID: fmt.Sprintf("run_held_shard_%d", i), Playbook: "site.yml",
				Status: run.StatusPendingApproval, CreatedAt: time.Now(), ParentID: &parentID,
				ShardIndex: &idx, ShardCount: &n,
			}); err != nil {
				t.Fatalf("Save(shard) error = %v", err)
			}
		}

		if _, err := d.Approve(ctx, parentID, "admin", "user"); err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
		waitForStatus(t, store, parentID, run.StatusFailed)
		parent, err := store.Get(ctx, parentID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if !strings.Contains(parent.Error, "only 2 of 3 shards") {
			t.Errorf("parent error = %q, want it to say shards are missing", parent.Error)
		}
		shards, err := store.Shards(ctx, parentID)
		if err != nil {
			t.Fatalf("Shards() error = %v", err)
		}
		for _, s := range shards {
			if s.Status != run.StatusCanceled {
				t.Errorf("shard %s is %s, want canceled", s.ID, s.Status)
			}
		}
		if n := runner.executions.Load(); n != 0 {
			t.Errorf("the runner ran %d times, want none", n)
		}
	})

	t.Run("retry", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		store := run.NewMemStore()
		d := New(store, &countingRunnerLister{}, nil, WithNoJanitor())
		defer d.Close()
		parentID := seedFailedSplit(t, store, 3, 2)
		before, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}

		got, err := d.RetryFailedShards(ctx, parentID)
		if !errors.Is(err, ErrIncompleteSplit) {
			t.Fatalf("RetryFailedShards() error = %v, want ErrIncompleteSplit", err)
		}
		if got != nil {
			t.Errorf("a refused retry returned run %s", got.ID)
		}
		after, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("the refused retry stored %d runs", len(after)-len(before))
		}
	})
}
