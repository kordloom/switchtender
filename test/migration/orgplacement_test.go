package migration

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestImportedSmartInventoryLandsInItsOrganization covers where the fixture's smart inventory
// lands. The AWX export carries the organization the smart inventory belongs to, so the import
// places the smart inventory and every inventory imported from that organization in one
// organization of the same name, and the smart inventory reads exactly those inventories, which is
// the reach it had in AWX. Before, it arrived with no organization and read every inventory on the
// install.
func TestImportedSmartInventoryLandsInItsOrganization(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("a")

	var orgs struct {
		// Orgs are the install's organizations.
		Orgs []struct {
			// ID is the organization's id.
			ID string `json:"id"`
			// Name is its name.
			Name string `json:"name"`
		} `json:"orgs"`
	}
	in.must(s, "admin", "GET", "/v1/orgs", nil, 200).decode(t, &orgs)
	if len(orgs.Orgs) != 1 || orgs.Orgs[0].Name != "Platform" {
		t.Fatalf("organizations after the import = %+v, want the one the export carries", orgs.Orgs)
	}
	platform := orgs.Orgs[0].ID

	var list struct {
		// Inventories are the stored inventories.
		Inventories []struct {
			// Name is the inventory's name.
			Name string `json:"name"`
			// OrgID is the organization it is placed in.
			OrgID string `json:"org_id"`
		} `json:"inventories"`
	}
	in.must(s, "admin", "GET", "/v1/inventories", nil, 200).decode(t, &list)
	var placed []string
	for _, inv := range list.Inventories {
		if inv.OrgID == platform {
			placed = append(placed, inv.Name)
		}
	}
	sorted := cmpopts.SortSlices(func(a, b string) bool { return a < b })
	if diff := cmp.Diff([]string{"edge", "fleet", "shut down", "web everywhere"}, placed,
		sorted); diff != "" {
		t.Errorf("inventories placed in the organization (-want +got):\n%s", diff)
	}

	var preview struct {
		// Hosts are the hosts the smart inventory resolves to for the operator.
		Hosts []string `json:"hosts"`
	}
	in.must(s, "operator", "POST", "/v1/inventories/"+in.lookup(s, "inventories",
		"web everywhere")+"/preview", nil, 200).decode(t, &preview)
	if diff := cmp.Diff([]string{"edge1", "web1", "web2"}, preview.Hosts, sorted); diff != "" {
		t.Errorf("hosts the placed smart inventory reaches (-want +got):\n%s", diff)
	}
}
