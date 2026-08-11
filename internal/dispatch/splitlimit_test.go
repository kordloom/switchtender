package dispatch

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// limitAwareLister is a Runner that lists hosts the way ansible-inventory does, returning only the
// hosts a limit matches. It records every limit it was asked for so a test can tell a lister that
// filtered from one that was never given the pattern.
type limitAwareLister struct {
	// byLimit maps a limit pattern to the hosts it matches. The empty key is the whole inventory.
	byLimit map[string][]string
	// asked records each limit passed to Hosts.
	asked []string
}

// Run reports success without doing anything.
func (l *limitAwareLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns the hosts matching limit, recording the pattern it was given.
func (l *limitAwareLister) Hosts(_ context.Context, _, limit string) ([]string, error) {
	l.asked = append(l.asked, limit)
	return l.byLimit[limit], nil
}

// shardHosts returns every host named across a split's stored shards, sorted.
func shardHosts(t *testing.T, store run.Store, parentID string) []string {
	t.Helper()
	children, err := store.Shards(context.Background(), parentID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	var hosts []string
	for _, c := range children {
		for _, h := range strings.Split(c.Limit, ",") {
			if h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	slices.Sort(hosts)
	return hosts
}

// TestSubmitSplitHonorsLimit checks a sharded run stays inside the host limit its caller asked for.
//
// A limit is a blast radius control, and sharding discarded it: the split listed the WHOLE inventory,
// partitioned that, and overwrote each shard's limit with its slice of the unfiltered list. A submit
// limited to the web tier fanned out across the database hosts and answered 202, so the first sign
// was the run touching hosts nobody asked for.
//
// The assertion is on the stored shards rather than on the returned parent, because the shards are
// what execute. Counting them would not catch this: the shard count was always right, and the hosts
// inside them were wrong.
func TestSubmitSplitHonorsLimit(t *testing.T) {
	t.Parallel()
	inventory := map[string][]string{
		"":     {"db1", "db2", "web1", "web2"},
		"web*": {"web1", "web2"},
		"web1": {"web1"},
	}

	t.Run("a limited split reaches only the hosts the limit matches", func(t *testing.T) {
		t.Parallel()
		store := run.NewMemStore()
		lister := &limitAwareLister{byLimit: inventory}
		d := New(store, lister, nil)
		defer d.Close()

		parent, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2,
			run.WithLimit("web*"))
		if err != nil {
			t.Fatalf("SubmitSplit() error = %v", err)
		}
		if diff := cmp.Diff([]string{"web1", "web2"}, shardHosts(t, store, parent.ID)); diff != "" {
			t.Errorf("shards reached hosts outside the limit (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"web*"}, lister.asked); diff != "" {
			t.Errorf("host listing was not narrowed (-want +got):\n%s", diff)
		}
	})

	t.Run("an unlimited split still reaches the whole inventory", func(t *testing.T) {
		t.Parallel()
		store := run.NewMemStore()
		d := New(store, &limitAwareLister{byLimit: inventory}, nil)
		defer d.Close()

		parent, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2)
		if err != nil {
			t.Fatalf("SubmitSplit() error = %v", err)
		}
		want := []string{"db1", "db2", "web1", "web2"}
		if diff := cmp.Diff(want, shardHosts(t, store, parent.ID)); diff != "" {
			t.Errorf("an unlimited split lost hosts (-want +got):\n%s", diff)
		}
	})

	t.Run("a limit matching one host runs once and keeps the pattern", func(t *testing.T) {
		t.Parallel()
		store := run.NewMemStore()
		d := New(store, &limitAwareLister{byLimit: inventory}, nil)
		defer d.Close()

		// Fewer than two hosts is not worth sharding, so this falls back to a single run. It must
		// still carry the limit, or the degenerate case becomes the fleet-wide run this test exists
		// to prevent.
		got, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2, run.WithLimit("web1"))
		if err != nil {
			t.Fatalf("SubmitSplit() error = %v", err)
		}
		if got.ShardCount != nil {
			t.Errorf("ShardCount = %v, want a single unsharded run", got.ShardCount)
		}
		if got.Limit != "web1" {
			t.Errorf("Limit = %q, want the caller's pattern to survive the fallback", got.Limit)
		}
	})
}
