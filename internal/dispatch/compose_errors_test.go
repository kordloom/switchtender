package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/run"
)

// downInventories is an inventory store whose Get fails for the ids in down and whose List fails
// when listDown is set.
type downInventories struct {
	inventory.Store
	// down holds the ids whose Get fails.
	down map[string]bool
	// listDown makes List fail.
	listDown bool
}

// Get fails for an id in down and reads the store otherwise.
func (s *downInventories) Get(ctx context.Context, id string) (*inventory.Inventory, error) {
	if s.down[id] {
		return nil, fmt.Errorf("get inventory %s: %w", id, errStoreDown)
	}
	return s.Store.Get(ctx, id)
}

// List fails when listDown is set and reads the store otherwise.
func (s *downInventories) List(ctx context.Context) ([]*inventory.Inventory, error) {
	if s.listDown {
		return nil, errStoreDown
	}
	return s.Store.List(ctx)
}

// downFacts is a run store whose fleet memory cannot be read.
type downFacts struct {
	run.Store
}

// EstateAt fails.
func (downFacts) EstateAt(context.Context, time.Time, int) ([]run.HostFacts, error) {
	return nil, errStoreDown
}

// faultyLister is a ListingRunner whose ansible-inventory fails or prints what it should not.
type faultyLister struct {
	*inventorytest.ListingRunner
	// versionErr, when set, is what reading the ansible-core version fails with.
	versionErr error
	// listErr, when set, is what every ansible-inventory --list run fails with.
	listErr error
	// listing, when set, is what every ansible-inventory --list run prints instead of a listing.
	listing string
}

// AnsibleCoreVersion fails with versionErr when it is set.
func (l *faultyLister) AnsibleCoreVersion(ctx context.Context) (string, error) {
	if l.versionErr != nil {
		return "", l.versionErr
	}
	return l.ListingRunner.AnsibleCoreVersion(ctx)
}

// ListInventory fails with listErr, or prints listing, when either is set.
func (l *faultyLister) ListInventory(ctx context.Context, sources []string, limit string) ([]byte, error) {
	if l.listErr != nil {
		return nil, l.listErr
	}
	if l.listing != "" {
		return []byte(l.listing), nil
	}
	return l.ListingRunner.ListInventory(ctx, sources, limit)
}

// TestComposeErrorsSayWhatFailed pins which errors composing an inventory ends in. A failure of
// this server or its stores carries none of the errors a submission is classed by, so it is
// answered as the server's failure and never as a sender's mistake a webhook would stop retrying.
// A failed read of an input's content carries ErrContentRead alone, and an input that is gone is
// the definition's to fix.
//
//nolint:funlen // Test function.
func TestComposeErrorsSayWhatFailed(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	failing, err := sealer.Seal("echo the vault is sealed >&2; exit 3")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	errAnsible := errors.New("ansible-inventory: exit status 1")
	errVersion := errors.New("ansible-inventory --version: signal: killed")
	smart := &inventory.Inventory{ID: "inv_s", Name: "picked", Kind: inventory.KindSmart,
		HostFilter: "groups__name=web"}
	constructed := &inventory.Inventory{ID: "inv_k", Name: "built", Kind: inventory.KindConstructed,
		InputIDs: []string{"inv_a"}}
	tests := []struct {
		Inv        *inventory.Inventory
		Extra      *inventory.Inventory
		Pinned     *run.InventoryResolution
		Access     run.InventoryAccessFunc
		Down       map[string]bool
		ListDown   bool
		FactsDown  bool
		VersionErr error
		ListErr    error
		Listing    string
		Want       error
		WantCause  error
	}{{ // Test 0: A smart inventory whose store cannot list its inputs is the server's failure.
		Inv: smart, ListDown: true, WantCause: errStoreDown,
	}, { // Test 1: A constructed inventory whose input the store cannot return is the server's.
		Inv: constructed, Down: map[string]bool{"inv_a": true}, WantCause: errStoreDown,
	}, { // Test 2: So is a held resolution whose input the store cannot return.
		Inv: smart, Down: map[string]bool{"inv_a": true}, WantCause: errStoreDown,
		Pinned: &run.InventoryResolution{Kind: inventory.KindSmart, Inputs: []string{"inv_a"},
			Hosts: []string{"web1"}},
	}, { // Test 3: So is fleet memory that cannot be read for a filter on gathered facts.
		Inv: &inventory.Inventory{ID: "inv_s", Name: "picked", Kind: inventory.KindSmart,
			HostFilter: "ansible_facts__ansible_distribution=Ubuntu"},
		FactsDown: true, WantCause: errStoreDown,
	}, { // Test 4: So is an access check that cannot answer.
		Inv: smart, WantCause: errStoreDown,
		Access: func(context.Context, string) (bool, error) { return false, errStoreDown },
	}, { // Test 5: So is an ansible-core version that cannot be read, which reads no inventory.
		Inv: constructed, VersionErr: errVersion, WantCause: errVersion,
	}, { // Test 6: So is ansible-inventory printing a listing this server cannot parse.
		Inv: constructed, Listing: "not a listing", WantCause: inventory.ErrListing,
	}, { // Test 7: ansible-inventory failing on a constructed inventory is a failed read of it.
		Inv: constructed, ListErr: errAnsible, Want: inventory.ErrContentRead, WantCause: errAnsible,
	}, { // Test 8: So is ansible-inventory failing on an input only Ansible can read.
		Inv: smart, Extra: &inventory.Inventory{ID: "inv_cloud", Name: "cloud", Content: pluginConfig},
		ListErr: errAnsible, Want: inventory.ErrContentRead, WantCause: errAnsible,
	}, { // Test 9: So is an input whose content source fails.
		Inv: &inventory.Inventory{ID: "inv_k", Name: "built", Kind: inventory.KindConstructed,
			InputIDs: []string{"inv_src"}},
		Extra: &inventory.Inventory{ID: "inv_src", Name: "sourced",
			ContentSource: credential.SourceCommand, ContentConfig: failing},
		Want: inventory.ErrContentRead,
	}, { // Test 10: A constructed inventory naming an input that is gone is the definition's to fix.
		Inv: &inventory.Inventory{ID: "inv_k", Name: "built", Kind: inventory.KindConstructed,
			InputIDs: []string{"inv_gone"}},
		Want: inventory.ErrComposition, WantCause: inventory.ErrNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			backing := composeInventories(t)
			if test.Extra != nil {
				if err := backing.Save(ctx, test.Extra); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			invs := &downInventories{Store: backing, down: test.Down, listDown: test.ListDown}
			store := run.NewMemStore()
			if test.FactsDown {
				store = downFacts{Store: store}
			}
			runner := &faultyLister{versionErr: test.VersionErr, listErr: test.ListErr,
				listing: test.Listing, ListingRunner: &inventorytest.ListingRunner{
					Canned: map[string]string{pluginConfig: pluginListing}}}
			d := New(store, runner, zap.NewNop(), WithInventories(invs),
				WithCredentials(credential.NewMemStore(), sealer))
			t.Cleanup(d.Close)
			var err error
			if test.Pinned != nil {
				_, err = d.composedContent(ctx, test.Inv, test.Pinned)
			} else {
				_, err = d.PreviewInventory(run.WithInventoryAccess(ctx, test.Access), test.Inv)
			}
			if err == nil {
				t.Fatal("composing succeeded, want an error")
			}
			for _, sentinel := range []error{inventory.ErrResolve, inventory.ErrComposition,
				inventory.ErrContentRead, ErrInventorySnapshot} {
				if sentinel != test.Want && errors.Is(err, sentinel) {
					t.Errorf("error %q carries %q", err, sentinel)
				}
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("error %q does not carry %q", err, test.Want)
			}
			if test.WantCause != nil && !errors.Is(err, test.WantCause) {
				t.Errorf("error %q lost its cause %q", err, test.WantCause)
			}
		})
	}
}

// TestSnapshotOfUnreadableContentSaysSo pins that a plain inventory whose content source fails when
// a run is submitted is refused as a failed read of its content, not as a snapshot this server
// could not take, which a submission answers as the server's failure.
func TestSnapshotOfUnreadableContentSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	failing, err := sealer.Seal("echo the vault is sealed >&2; exit 3")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	invs := inventory.NewMemStore()
	if err := invs.Save(ctx, &inventory.Inventory{ID: "inv_src", Name: "sourced",
		ContentSource: credential.SourceCommand, ContentConfig: failing}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithInventories(invs),
		WithCredentials(credential.NewMemStore(), sealer))
	t.Cleanup(d.Close)
	err = d.snapshotInventory(ctx, &run.Run{ID: "run_1", InventoryID: "inv_src"})
	if !errors.Is(err, inventory.ErrContentRead) {
		t.Errorf("snapshotInventory() error = %v, want ErrContentRead", err)
	}
	if errors.Is(err, ErrInventorySnapshot) {
		t.Errorf("snapshotInventory() error = %v carries ErrInventorySnapshot", err)
	}
}
