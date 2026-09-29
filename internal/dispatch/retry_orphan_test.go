package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestRetryFailedShardsRecoversOrphans proves a shard that was still running when the split
// coordinator died is retried after a crash, even though its executor finalized it canceled with an
// empty error, indistinguishable by error text from a user cancel. An interrupted parent is the
// authoritative signal of coordinator death, so every non-succeeded shard under one is an orphan to
// retry. A shard a person canceled on a healthy parent is not swept up.
func TestRetryFailedShardsRecoversOrphans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		Name         string
		ParentStatus run.Status
		Shards       []*run.Run
		WantLimits   []string
		Want         error
	}{{ // Test 0: A mid-flight shard on an interrupted parent, canceled with an empty error, is retried.
		Name:         "mid-flight orphan on interrupted parent",
		ParentStatus: run.StatusInterrupted,
		Shards: []*run.Run{
			{Status: run.StatusCanceled, Error: "", Limit: "host-a"},
			{Status: run.StatusSucceeded, Limit: "host-b"},
		},
		WantLimits: []string{"host-a"},
	}, { // Test 1: A shard the sweep canceled before it started, stamped with the orphan error, is still retried.
		Name:         "stamped orphan on interrupted parent",
		ParentStatus: run.StatusInterrupted,
		Shards: []*run.Run{
			{Status: run.StatusCanceled, Error: run.OrphanError(), Limit: "host-a"},
			{Status: run.StatusSucceeded, Limit: "host-b"},
		},
		WantLimits: []string{"host-a"},
	}, { // Test 2: On a healthy failed parent only the failed shard runs again, not the user-canceled one.
		Name:         "user cancel on healthy parent is not retried",
		ParentStatus: run.StatusFailed,
		Shards: []*run.Run{
			{Status: run.StatusCanceled, Error: "", Limit: "host-a"},
			{Status: run.StatusFailed, Limit: "host-b"},
		},
		WantLimits: []string{"host-b"},
	}, { // Test 3: A healthy parent whose only non-succeeded shard was user-canceled has nothing to retry.
		Name:         "only a user cancel on a healthy parent",
		ParentStatus: run.StatusFailed,
		Shards: []*run.Run{
			{Status: run.StatusCanceled, Error: "", Limit: "host-a"},
			{Status: run.StatusSucceeded, Limit: "host-b"},
		},
		Want: ErrNoFailedShards,
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, &fakeRunnerLister{hosts: []string{"host-a", "host-b"}}, nil)
			defer d.Close()

			parentID := fmt.Sprintf("parent_%d", testNum)
			parent := &run.Run{
				ID: parentID, Kind: run.KindSplit, Status: test.ParentStatus,
				Playbook: "play.yml", Inventory: "inv",
			}
			if err := store.Save(ctx, parent); err != nil {
				t.Fatalf("Save(parent) error = %v", err)
			}
			for i, s := range test.Shards {
				idx, count := i, len(test.Shards)
				s.ID = fmt.Sprintf("%s_shard_%d", parentID, i)
				s.ParentID = &parentID
				s.ShardIndex = &idx
				s.ShardCount = &count
				if err := store.Save(ctx, s); err != nil {
					t.Fatalf("Save(shard) error = %v", err)
				}
			}

			retry, err := d.RetryFailedShards(ctx, parentID)
			if !errors.Is(err, test.Want) {
				t.Fatalf("RetryFailedShards() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			children, err := store.Shards(ctx, retry.ID)
			if err != nil {
				t.Fatalf("Shards(retry) error = %v", err)
			}
			var gotLimits []string
			for _, c := range children {
				gotLimits = append(gotLimits, c.Limit)
			}
			sort.Strings(gotLimits)
			if diff := cmp.Diff(test.WantLimits, gotLimits); diff != "" {
				t.Errorf("retried shard host groups (-want +got):\n%s", diff)
			}
		})
	}
}
