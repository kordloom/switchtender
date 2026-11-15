package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// pathLister is a Runner that lists the same hosts for every inventory and records each inventory
// path it was asked to list, by either lister function, so a test can tell whether a listing was
// reached and whether a path a caller wrote reached it.
type pathLister struct {
	// mu guards paths.
	mu sync.Mutex
	// paths records the inventory argument of each Hosts call.
	paths []string
}

// Run reports success without doing anything.
func (l *pathLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns four hosts, recording the inventory path it was given.
func (l *pathLister) Hosts(_ context.Context, path, _ string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, path)
	return []string{"a", "b", "c", "d"}, nil
}

// ListInventory records the sources it was given and returns an empty listing.
func (l *pathLister) ListInventory(_ context.Context, sources []string, _ string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, sources...)
	return []byte(`{"_meta":{"hostvars":{}}}`), nil
}

// listed returns how many listings were asked for and whether path was one of them.
func (l *pathLister) listed(path string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.paths), slices.Contains(l.paths, path)
}

// TestSubmitSplitRefusesBeforeListing pins the two refusals a split makes ahead of listing hosts.
//
// A split lists its inventory before the run is validated, gated, or held, and with no stored
// inventory what it lists is the path the caller wrote, so the listing itself answered whether a
// file exists on the server. The callers that take such a split admit admins only, and the
// dispatcher refuses the one caller that is never an admin, a run marked as an agent's, without
// learning anything about roles. A limit that holds an at sign is refused at the same point, since
// the listing is handed the limit too and Ansible opens what follows the sign as a file.
//
//nolint:funlen // Test function.
func TestSubmitSplitRefusesBeforeListing(t *testing.T) {
	t.Parallel()
	const rawPath = "/srv/inventories/prod.ini"
	tests := []struct {
		// Want is the expected error.
		Want error
		// Opts say who submits and with what limit.
		Opts []run.SubmitOption
		// Stored splits a stored inventory in place of the path.
		Stored bool
		// Shards is the shard count asked for.
		Shards int
		// WantListings is how many host listings the submit asks for.
		WantListings int
		// WantRawListed is whether the path the caller wrote was listed.
		WantRawListed bool
	}{{ // Test 0: An agent's split of a path is refused and nothing is listed.
		Opts: dispatchtest.AgentOpts(), Shards: 2, Want: ErrAgentPathSplit,
	}, { // Test 1: A person's split of a path lists it, since the caller checked the role.
		Opts: dispatchtest.PersonOpts(), Shards: 2, WantListings: 1, WantRawListed: true,
	}, { // Test 2: A split with no actor, as a schedule or the demo seed sends, lists the path.
		Shards: 2, WantListings: 1, WantRawListed: true,
	}, { // Test 3: A webhook's split of a path lists it.
		Opts:   []run.SubmitOption{run.WithActor("push"), run.WithActorType("webhook")},
		Shards: 2, WantListings: 1, WantRawListed: true,
	}, { // Test 4: An agent's split of a stored inventory lists that inventory's own file.
		Opts: dispatchtest.AgentOpts(), Stored: true, Shards: 2, WantListings: 1,
	}, { // Test 5: An agent's single run names a path without listing anything.
		Opts: dispatchtest.AgentOpts(), Shards: 1,
	}, { // Test 6: A limit with a file after a comma is refused before the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("nosuchhost,@/etc/shells")),
		Shards: 2, Want: run.ErrLimitFile,
	}, { // Test 7: A limit with a file after a colon is refused before the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("web:@/etc/shells")),
		Shards: 2, Want: run.ErrLimitFile,
	}, { // Test 8: The same limit on a single run is refused when the run is validated.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("nosuchhost,@/etc/shells")),
		Shards: 1, Want: run.ErrLimitFile,
	}, { // Test 9: A limit of hosts is handed to the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("a,b,c")),
		Shards: 2, WantListings: 1, WantRawListed: true,
	}, { // Test 10: A group pattern is handed to the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("web:!canary:&east")),
		Shards: 2, WantListings: 1, WantRawListed: true,
	}, { // Test 11: A file after a space is refused before the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("web01 @/nonexistent/x")),
		Shards: 2, Want: run.ErrLimitFile,
	}, { // Test 12: A file after a closing bracket is refused before the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("web01]@/nonexistent/x")),
		Shards: 2, Want: run.ErrLimitFile,
	}, { // Test 13: A file after an opening bracket is refused before the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("web01[@/nonexistent/x")),
		Shards: 2, Want: run.ErrLimitFile,
	}, { // Test 14: A file behind U+001C is refused before the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("\x1c@/nonexistent/x")),
		Shards: 2, Want: run.ErrLimitFile,
	}, { // Test 15: A file after a comma, the path absent, is refused before the listing.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("web01,@/nonexistent/x")),
		Shards: 2, Want: run.ErrLimitFile,
	}, { // Test 16: A file after a space on a single run is refused when the run is validated.
		Opts:   append(dispatchtest.PersonOpts(), run.WithLimit("web01 @/nonexistent/x")),
		Shards: 1, Want: run.ErrLimitFile,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			lister := &pathLister{}
			invs := inventory.NewMemStore()
			fleet := &inventory.Inventory{ID: "inv_fleet", Name: "fleet", Content: "a\nb\nc\nd\n"}
			if err := invs.Save(ctx, fleet); err != nil {
				t.Fatalf("invs.Save() error = %v", err)
			}
			d := New(store, lister, zap.NewNop(), WithNoJanitor(), WithInventories(invs))
			t.Cleanup(d.Close)

			opts, path := slices.Clone(test.Opts), rawPath
			if test.Stored {
				opts, path = append(opts, run.WithInventory(fleet.ID)), ""
			}
			created, err := d.SubmitSplit(ctx, "play.yml", path, test.Shards, opts...)
			if !errors.Is(err, test.Want) {
				t.Fatalf("SubmitSplit() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil && created != nil {
				t.Errorf("a refused submit returned run %s", created.ID)
			}
			listings, rawListed := lister.listed(rawPath)
			if diff := cmp.Diff(test.WantListings, listings); diff != "" {
				t.Errorf("host listings mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantRawListed, rawListed); diff != "" {
				t.Errorf("caller's path listed mismatch (-want +got):\n%s", diff)
			}
			if test.Want == nil {
				return
			}
			stored, lerr := store.List(ctx)
			if lerr != nil {
				t.Fatalf("store.List() error = %v", lerr)
			}
			if len(stored) != 0 {
				t.Errorf("a refused submit stored %d runs, want none", len(stored))
			}
		})
	}
}
