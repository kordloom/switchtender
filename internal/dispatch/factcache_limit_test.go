package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
)

// TestFactCacheServesTheHostsALimitReaches pins which hosts' cached facts a limited run is served.
// A limit that names hosts outright reads only those, the way one host's provisioning callback
// should. A limit that names a group reads every host's, because the group may reach any of them:
// reading the group's name as a host found nothing, so a template limited to a group ran with no
// cached facts at all while its fact cache looked on.
func TestFactCacheServesTheHostsALimitReaches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Limit      string
		WantServed []string
	}{{ // Test 0: One host reads that host alone.
		Limit: "web01", WantServed: []string{"web01"},
	}, { // Test 1: A group reads every host it may reach.
		Limit: "web", WantServed: []string{"db01", "web01", "web02"},
	}, { // Test 2: Hosts and a group together read every host.
		Limit: "db01,web", WantServed: []string{"db01", "web01", "web02"},
	}, { // Test 3: A list of hosts reads those hosts.
		Limit: "web02,db01", WantServed: []string{"db01", "web02"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			facts := factcache.NewMemStore()
			inventories := inventory.NewMemStore()
			if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
				Content: "[web]\nweb01\nweb02\n[db]\ndb01\n", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			var cached []factcache.Entry
			for _, host := range []string{"web01", "web02", "db01"} {
				cached = append(cached, factcache.Entry{InventoryID: "inv_1", Host: host,
					Facts:      json.RawMessage(`{"ansible_hostname":"` + host + `"}`),
					ModifiedAt: time.Now()})
			}
			if err := facts.SaveFacts(ctx, cached); err != nil {
				t.Fatal(err)
			}
			play := &factPlay{}
			d := New(store, play.runner(), nil, WithNoJanitor(), WithInventories(inventories),
				WithFactCache(facts))
			defer d.Close()
			r, err := d.Submit(ctx, "site.yml", "", run.WithInventory("inv_1"),
				run.WithFactCache(true, 0), run.WithLimit(test.Limit))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			waitTerminal(t, store, r.ID)
			served, _ := play.last()
			if diff := cmp.Diff(test.WantServed, hostsOf(served)); diff != "" {
				t.Errorf("served hosts for limit %q mismatch (-want +got):\n%s", test.Limit, diff)
			}
		})
	}
}
