// Package factcachetest provides a shared behavior contract for factcache.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package factcachetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
)

// Contract runs the factcache.Store contract against a fresh store from newStores, which returns
// the fact store and the inventory store beside it. The inventories the contract's facts belong to
// are stored first, since a store that keeps inventories beside the facts writes none for an
// inventory it does not hold. A fact store with no inventories beside it is paired with
// inventory.NewMemStore.
func Contract(t *testing.T, newStores func() (factcache.Store, inventory.Store)) {
	t.Helper()
	newStore := func() factcache.Store {
		facts, inventories := newStores()
		storeInventories(t, inventories, "inv_1", "inv_2", "inv_other")
		return facts
	}
	t.Run("round trip", func(t *testing.T) { testRoundTrip(t, newStore()) })
	t.Run("replace", func(t *testing.T) { testReplace(t, newStore()) })
	t.Run("list", func(t *testing.T) { testList(t, newStore()) })
	t.Run("clear", func(t *testing.T) { testClear(t, newStore()) })
	t.Run("refusals", func(t *testing.T) { testRefusals(t, newStore()) })
	t.Run("older gather keeps newer facts", func(t *testing.T) { testOlderGather(t, newStore()) })
}

// storeInventories stores an inventory under each id, for the facts the contract saves to belong
// to.
func storeInventories(t *testing.T, inventories inventory.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := inventories.Save(context.Background(), &inventory.Inventory{ID: id, Name: id,
			Content: "web01", CreatedAt: at}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
}

// testOlderGather verifies a save whose facts were gathered before the stored ones leaves the
// stored ones in place, and that a save stamped the same instant replaces them. A run that gathered
// first and finished last used to write its older facts over newer ones and stamp them as the
// newest.
func testOlderGather(t *testing.T, store factcache.Store) {
	ctx := context.Background()
	tests := []struct {
		Doc      string
		At       time.Time
		WantDoc  string
		WantTime time.Time
	}{{ // Test 0: The first save is stored.
		Doc: `{"kernel":"6.8.0"}`, At: at.Add(time.Hour), WantDoc: `{"kernel":"6.8.0"}`,
		WantTime: at.Add(time.Hour),
	}, { // Test 1: Facts gathered earlier, saved later, do not replace them.
		Doc: `{"kernel":"6.1.0"}`, At: at, WantDoc: `{"kernel":"6.8.0"}`, WantTime: at.Add(time.Hour),
	}, { // Test 2: Facts gathered a moment earlier inside the same second do not either.
		Doc: `{"kernel":"6.2.0"}`, At: at.Add(time.Hour - time.Millisecond),
		WantDoc: `{"kernel":"6.8.0"}`, WantTime: at.Add(time.Hour),
	}, { // Test 3: Facts stamped the same instant replace them.
		Doc: `{"kernel":"6.9.0"}`, At: at.Add(time.Hour), WantDoc: `{"kernel":"6.9.0"}`,
		WantTime: at.Add(time.Hour),
	}, { // Test 4: Facts gathered later replace them.
		Doc: `{"kernel":"7.0.0"}`, At: at.Add(2 * time.Hour), WantDoc: `{"kernel":"7.0.0"}`,
		WantTime: at.Add(2 * time.Hour),
	}}
	for testNum, test := range tests {
		if err := store.SaveFacts(ctx, []factcache.Entry{{InventoryID: "inv_1", Host: "web01",
			Facts: json.RawMessage(test.Doc), RunID: fmt.Sprintf("run_%d", testNum),
			ModifiedAt: test.At}}); err != nil {
			t.Fatalf("test %d: SaveFacts() error = %v", testNum, err)
		}
		got, err := store.Facts(ctx, "inv_1", "web01")
		if err != nil {
			t.Fatalf("test %d: Facts() error = %v", testNum, err)
		}
		if string(got.Facts) != test.WantDoc || !got.ModifiedAt.Equal(test.WantTime) {
			t.Errorf("test %d: stored %s gathered %v, want %s gathered %v", testNum, got.Facts,
				got.ModifiedAt, test.WantDoc, test.WantTime)
		}
	}
}

// at is a fixed instant with sub-second precision, so a store that truncates or shifts time fails.
var at = time.Date(2026, 9, 30, 12, 34, 56, 789000000, time.UTC)

// testRoundTrip verifies a saved entry reads back with its document, size, run, and time intact,
// and that an unknown host reports ErrNotFound.
func testRoundTrip(t *testing.T, store factcache.Store) {
	ctx := context.Background()
	if _, err := store.Facts(ctx, "inv_1", "web01"); !errors.Is(err, factcache.ErrNotFound) {
		t.Fatalf("Facts(unknown) error = %v, want ErrNotFound", err)
	}
	doc := json.RawMessage(`{"ansible_distribution":"Debian","ansible_env":{"HOME":"/root"},"n":1}`)
	if err := store.SaveFacts(ctx, []factcache.Entry{{
		InventoryID: "inv_1", Host: "web01", Facts: doc, RunID: "run_1", ModifiedAt: at,
	}}); err != nil {
		t.Fatalf("SaveFacts() error = %v", err)
	}
	got, err := store.Facts(ctx, "inv_1", "web01")
	if err != nil {
		t.Fatalf("Facts() error = %v", err)
	}
	want := &factcache.Entry{
		InventoryID: "inv_1", Host: "web01", Facts: doc, Bytes: len(doc), RunID: "run_1",
		ModifiedAt: at,
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Facts() mismatch (-want +got):\n%s", diff)
	}
	if _, err := store.Facts(ctx, "inv_other", "web01"); !errors.Is(err, factcache.ErrNotFound) {
		t.Errorf("Facts(other inventory) error = %v, want ErrNotFound: facts are per inventory", err)
	}
}

// testReplace verifies a second save for the same host replaces the first.
func testReplace(t *testing.T, store factcache.Store) {
	ctx := context.Background()
	for i, doc := range []string{`{"v":1}`, `{"v":2}`} {
		if err := store.SaveFacts(ctx, []factcache.Entry{{
			InventoryID: "inv_1", Host: "db01", Facts: json.RawMessage(doc),
			RunID: fmt.Sprintf("run_%d", i), ModifiedAt: at.Add(time.Duration(i) * time.Hour),
		}}); err != nil {
			t.Fatalf("SaveFacts(%d) error = %v", i, err)
		}
	}
	got, err := store.Facts(ctx, "inv_1", "db01")
	if err != nil {
		t.Fatalf("Facts() error = %v", err)
	}
	second := at.Add(time.Hour)
	if string(got.Facts) != `{"v":2}` || got.RunID != "run_1" || !got.ModifiedAt.Equal(second) {
		t.Errorf("Facts() = %s from %s at %v, want the second save", got.Facts, got.RunID, got.ModifiedAt)
	}
	list, err := store.List(ctx, "inv_1", false)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Errorf("List() = %d entries, want 1 after a replace", len(list))
	}
}

// testList verifies a listing is ordered by host, scoped to its inventory, and carries documents
// only when asked.
func testList(t *testing.T, store factcache.Store) {
	ctx := context.Background()
	entries := []factcache.Entry{
		{InventoryID: "inv_1", Host: "web02", Facts: json.RawMessage(`{"a":2}`), RunID: "run_1",
			ModifiedAt: at},
		{InventoryID: "inv_1", Host: "web01", Facts: json.RawMessage(`{"a":1}`), RunID: "run_1",
			ModifiedAt: at},
		{InventoryID: "inv_2", Host: "web01", Facts: json.RawMessage(`{"b":1}`), RunID: "run_2",
			ModifiedAt: at},
	}
	if err := store.SaveFacts(ctx, entries); err != nil {
		t.Fatalf("SaveFacts() error = %v", err)
	}
	tests := []struct {
		WantHosts []string
		WantFacts []string
		Inventory string
		WithFacts bool
	}{{ // Test 0: A summary listing carries sizes but no documents.
		Inventory: "inv_1", WantHosts: []string{"web01", "web02"}, WantFacts: []string{"", ""},
	}, { // Test 1: A full listing carries each document.
		Inventory: "inv_1", WithFacts: true, WantHosts: []string{"web01", "web02"},
		WantFacts: []string{`{"a":1}`, `{"a":2}`},
	}, { // Test 2: Another inventory's hosts are its own.
		Inventory: "inv_2", WithFacts: true, WantHosts: []string{"web01"}, WantFacts: []string{`{"b":1}`},
	}, { // Test 3: An inventory with nothing cached lists nothing.
		Inventory: "inv_none",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got, err := store.List(ctx, test.Inventory, test.WithFacts)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var hosts, facts []string
			for _, e := range got {
				hosts = append(hosts, e.Host)
				facts = append(facts, string(e.Facts))
				if e.Bytes == 0 {
					t.Errorf("List() entry %s has no size", e.Host)
				}
			}
			if diff := cmp.Diff(test.WantHosts, hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("List() hosts mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantFacts, facts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("List() facts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// testClear verifies clearing hosts and whole inventories removes exactly what it names.
func testClear(t *testing.T, store factcache.Store) {
	ctx := context.Background()
	var entries []factcache.Entry
	for _, inv := range []string{"inv_1", "inv_2"} {
		for _, h := range []string{"a", "b", "c"} {
			entries = append(entries, factcache.Entry{InventoryID: inv, Host: h,
				Facts: json.RawMessage(`{}`), ModifiedAt: at})
		}
	}
	if err := store.SaveFacts(ctx, entries); err != nil {
		t.Fatalf("SaveFacts() error = %v", err)
	}
	n, err := store.Clear(ctx, "inv_1", "a", "missing")
	if err != nil || n != 1 {
		t.Fatalf("Clear() = %d, %v, want 1 removed", n, err)
	}
	if _, err := store.Facts(ctx, "inv_1", "a"); !errors.Is(err, factcache.ErrNotFound) {
		t.Errorf("Facts(cleared) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Facts(ctx, "inv_2", "a"); err != nil {
		t.Errorf("Facts(same host, other inventory) error = %v, want it kept", err)
	}
	n, err = store.ClearInventory(ctx, "inv_1")
	if err != nil || n != 2 {
		t.Fatalf("ClearInventory() = %d, %v, want 2 removed", n, err)
	}
	left, err := store.List(ctx, "inv_2", false)
	if err != nil || len(left) != 3 {
		t.Errorf("List(inv_2) = %d, %v, want the other inventory untouched", len(left), err)
	}
}

// testRefusals verifies an invalid entry is refused, and that a batch with one invalid entry
// writes nothing at all.
func testRefusals(t *testing.T, store factcache.Store) {
	ctx := context.Background()
	big := json.RawMessage(`{"x":"` + strings.Repeat("a", factcache.MaxFactsBytes) + `"}`)
	tests := []struct {
		Want  error
		Entry factcache.Entry
	}{{ // Test 0: A document past the size bound.
		Entry: factcache.Entry{InventoryID: "inv_1", Host: "big", Facts: big, ModifiedAt: at},
		Want:  factcache.ErrTooLarge,
	}, { // Test 1: A document that is not an object.
		Entry: factcache.Entry{InventoryID: "inv_1", Host: "arr", Facts: json.RawMessage(`[1]`),
			ModifiedAt: at},
		Want: factcache.ErrInvalid,
	}, { // Test 2: A host name that would escape the cache directory.
		Entry: factcache.Entry{InventoryID: "inv_1", Host: "../etc", Facts: json.RawMessage(`{}`),
			ModifiedAt: at},
		Want: factcache.ErrInvalid,
	}, { // Test 3: No inventory.
		Entry: factcache.Entry{Host: "web01", Facts: json.RawMessage(`{}`), ModifiedAt: at},
		Want:  factcache.ErrInvalid,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			good := factcache.Entry{InventoryID: "inv_1", Host: fmt.Sprintf("good%d", testNum),
				Facts: json.RawMessage(`{}`), ModifiedAt: at}
			err := store.SaveFacts(ctx, []factcache.Entry{good, test.Entry})
			if !errors.Is(err, test.Want) {
				t.Fatalf("SaveFacts() error = %v, want %v", err, test.Want)
			}
			if _, err := store.Facts(ctx, "inv_1", good.Host); !errors.Is(err, factcache.ErrNotFound) {
				t.Errorf("Facts(%s) error = %v, want nothing written from a refused batch", good.Host, err)
			}
		})
	}
}

// InventoryCascade checks that deleting a stored inventory deletes the facts cached for its hosts
// and leaves every other inventory's facts alone, and that a save that arrives after the delete,
// the way a run that outlived its inventory collects its facts, writes nothing for the deleted
// inventory and the rest of its batch all the same. The two stores must share one database.
func InventoryCascade(t *testing.T, inventories inventory.Store, facts factcache.Store) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"inv_gone", "inv_kept"} {
		if err := inventories.Save(ctx, &inventory.Inventory{ID: id, Name: id, Content: "web01",
			CreatedAt: at}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		if err := facts.SaveFacts(ctx, []factcache.Entry{{InventoryID: id, Host: "web01",
			Facts: json.RawMessage(`{"secret":"x"}`), ModifiedAt: at}}); err != nil {
			t.Fatalf("SaveFacts(%s) error = %v", id, err)
		}
	}
	if err := inventories.Delete(ctx, "inv_gone"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := facts.Facts(ctx, "inv_gone", "web01"); !errors.Is(err, factcache.ErrNotFound) {
		t.Errorf("Facts(deleted inventory) error = %v, want ErrNotFound: the facts outlived it", err)
	}
	if _, err := facts.Facts(ctx, "inv_kept", "web01"); err != nil {
		t.Errorf("Facts(other inventory) error = %v, want it kept", err)
	}
	if err := facts.SaveFacts(ctx, []factcache.Entry{
		{InventoryID: "inv_gone", Host: "web01", Facts: json.RawMessage(`{"secret":"y"}`),
			ModifiedAt: at.Add(time.Hour)},
		{InventoryID: "inv_kept", Host: "web02", Facts: json.RawMessage(`{"os":"Debian"}`),
			ModifiedAt: at.Add(time.Hour)},
	}); err != nil {
		t.Fatalf("SaveFacts() after the delete error = %v, want the deleted inventory's entry "+
			"dropped without an error", err)
	}
	if _, err := facts.Facts(ctx, "inv_gone", "web01"); !errors.Is(err, factcache.ErrNotFound) {
		t.Errorf("Facts(deleted inventory) after a later save error = %v, want ErrNotFound: the "+
			"deleted inventory's facts came back", err)
	}
	if _, err := facts.Facts(ctx, "inv_kept", "web02"); err != nil {
		t.Errorf("Facts(stored inventory) from the same batch error = %v, want it written", err)
	}
}
