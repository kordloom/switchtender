package importer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// placementInventories is the inventory part of an awxkit-shaped export with two organizations:
// Ops holds an inventory with a nested dynamic source, a top-level dynamic source feeding that
// inventory, and a smart inventory, and Dev holds an inventory and no smart inventory.
const placementInventories = `
  "inventory": [
    {"name": "web fleet", "organization": {"name": "Ops"},
     "related": {"hosts": [{"name": "web1"}, {"name": "db1"}],
       "inventory_sources": [{"name": "ops cloud", "source": "scm",
         "source_path": "aws_ec2.yml"}]}},
    {"name": "dev fleet", "organization": {"name": "Dev"},
     "related": {"hosts": [{"name": "web9"}]}},
    {"name": "all web", "kind": "smart", "organization": {"name": "Ops"},
     "host_filter": "name__startswith=web"}
  ],
  "inventory_sources": [{"name": "ops extra", "source": "scm", "source_path": "extra.yml",
    "inventory": {"name": "web fleet", "organization": {"name": "Ops"}}}]`

// placementExport carries both organizations, so the smart inventory's own can be placed.
const placementExport = `{"organizations": [{"name": "Ops"}, {"name": "Dev"}],` +
	placementInventories + `}`

// placementExportNoOrgs names the organizations on each inventory without carrying them.
const placementExportNoOrgs = `{` + placementInventories + `}`

// placementExportAPI is the shape the REST API writes: organizations carry their ids and every
// reference to one is that integer.
const placementExportAPI = `{
  "organizations": [{"id": 7, "name": "Ops"}],
  "inventory": [
    {"name": "web fleet", "organization": 7, "hosts": [{"name": "web1"}]},
    {"name": "all web", "kind": "smart", "organization": 7,
     "host_filter": "name__startswith=web"}
  ]
}`

// placementStores returns empty stores for an import to apply into, with orgs as the organization
// store, which may be nil.
func placementStores(orgs org.Store) importer.ApplyStores {
	return importer.ApplyStores{
		Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
		Sources: invsource.NewMemStore(), Credentials: credential.NewMemStore(),
		Templates: template.NewMemStore(), Schedules: schedule.NewMemStore(), Orgs: orgs,
	}
}

// storedPlacement reads every stored inventory back and maps its name to the name of the
// organization it is stored in, empty for one stored in none.
func storedPlacement(t *testing.T, stores importer.ApplyStores) map[string]string {
	t.Helper()
	ctx := context.Background()
	names := map[string]string{}
	if stores.Orgs != nil {
		orgs, err := stores.Orgs.List(ctx)
		if err != nil {
			t.Fatalf("List() orgs error = %v", err)
		}
		for _, o := range orgs {
			names[o.ID] = o.Name
		}
	}
	invs, err := stores.Inventories.List(ctx)
	if err != nil {
		t.Fatalf("List() inventories error = %v", err)
	}
	out := map[string]string{}
	for _, inv := range invs {
		if inv.OrgID != "" && names[inv.OrgID] == "" {
			t.Errorf("inventory %q names organization %s, which is not stored", inv.Name, inv.OrgID)
		}
		out[inv.Name] = names[inv.OrgID]
	}
	return out
}

// TestAWXSmartInventoryPlacement pins where an imported smart inventory lands. When the export
// carries its organization, the smart inventory and every inventory imported from that organization
// are placed in it, created or matched by name when the import is applied. Without the organization
// in the export, or with a name that matches more than one organization here, nothing is placed and
// the warning says why. Before, every smart inventory arrived with no organization and so filtered
// every inventory on the install, a wider reach than it had in AWX.
func TestAWXSmartInventoryPlacement(t *testing.T) {
	t.Parallel()
	opsPlaced := map[string]string{
		"web fleet": "Ops", "all web": "Ops", "ops cloud (dynamic)": "Ops",
		"ops extra (dynamic)": "Ops", "dev fleet": "",
	}
	unplaced := map[string]string{
		"web fleet": "", "all web": "", "ops cloud (dynamic)": "", "ops extra (dynamic)": "",
		"dev fleet": "",
	}
	tests := []struct {
		Export string
		// Existing names the organizations already stored before the import is applied.
		Existing []string
		// NoOrgStore applies without an organization store.
		NoOrgStore     bool
		WantPlanOrgs   []string
		WantPlaced     map[string]string
		WantOrgs       []string
		WantCreatedOrg bool
		WantWarns      []string
	}{{ // Test 0: The export carries the organization, so the smart inventory, its organization's
		// inventories, and both sources' inventories are placed in a new organization of that name,
		// while the other organization's inventory is left as it was.
		Export: placementExport, WantPlanOrgs: []string{"Ops"}, WantPlaced: opsPlaced,
		WantOrgs: []string{"Ops"}, WantCreatedOrg: true,
		WantWarns: []string{`smart inventory "all web" is placed in organization "Ops" with the ` +
			`3 other inventories imported from that organization`,
			"this export holds 1 other organization, which is not imported"},
	}, { // Test 1: The export names the organization without carrying it, so nothing is placed and
		// the warning that has always said so still does.
		Export: placementExportNoOrgs, WantPlaced: unplaced,
		WantWarns: []string{`smart inventory "all web" filtered the hosts of organization "Ops" in ` +
			`AWX. Here a smart inventory with no organization filters every inventory`},
	}, { // Test 2: One organization of the name already exists here, so it is used and no second
		// one is made.
		Export: placementExport, Existing: []string{"Ops"}, WantPlanOrgs: []string{"Ops"},
		WantPlaced: opsPlaced, WantOrgs: []string{"Ops"},
		WantWarns: []string{`organization "Ops" already exists here`},
	}, { // Test 3: Two organizations here share the name, so none is guessed at and the smart
		// inventory arrives with no organization, saying why.
		Export: placementExport, Existing: []string{"Ops", "Ops"}, WantPlanOrgs: []string{"Ops"},
		WantPlaced: unplaced, WantOrgs: []string{"Ops", "Ops"},
		WantWarns: []string{`It arrives with no organization because 2 organizations here are ` +
			`named "Ops"`},
	}, { // Test 4: An export from the REST API refers to the organization by id, which resolves
		// through the organization the export carries under that id.
		Export: placementExportAPI, WantPlanOrgs: []string{"Ops"},
		WantPlaced: map[string]string{"web fleet": "Ops", "all web": "Ops"},
		WantOrgs:   []string{"Ops"}, WantCreatedOrg: true,
	}, { // Test 5: With no organization store nothing can be placed, and the warning says so.
		Export: placementExport, NoOrgStore: true, WantPlanOrgs: []string{"Ops"},
		WantPlaced: unplaced,
		WantWarns: []string{"It arrives with no organization because organizations are not " +
			"enabled on this install"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			plan, err := importer.FromAWX([]byte(test.Export), time.Date(2026, 9, 1, 0, 0, 0, 0,
				time.UTC))
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			var planOrgs []string
			for _, o := range plan.Orgs {
				planOrgs = append(planOrgs, o.Name)
			}
			if diff := cmp.Diff(test.WantPlanOrgs, planOrgs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("planned organizations mismatch (-want +got):\n%s", diff)
			}
			var orgs org.Store
			if !test.NoOrgStore {
				orgs = org.NewMemStore()
				for n, name := range test.Existing {
					if err := orgs.Save(ctx, &org.Org{ID: fmt.Sprintf("org_old%d", n), Name: name,
						CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
						t.Fatalf("Save() error = %v", err)
					}
				}
			}
			stores := placementStores(orgs)
			created, err := plan.Apply(ctx, stores)
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if diff := cmp.Diff(test.WantPlaced, storedPlacement(t, stores)); diff != "" {
				t.Errorf("stored placement mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantPlaced, withUnplaced(plan, test.WantPlaced)); diff != "" {
				t.Errorf("the plan's account of the placement mismatch (-want +got):\n%s", diff)
			}
			var gotOrgs []string
			if orgs != nil {
				list, err := orgs.List(ctx)
				if err != nil {
					t.Fatalf("List() error = %v", err)
				}
				for _, o := range list {
					gotOrgs = append(gotOrgs, o.Name)
				}
			}
			if diff := cmp.Diff(test.WantOrgs, gotOrgs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored organizations mismatch (-want +got):\n%s", diff)
			}
			invs, err := stores.Inventories.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			wantCreated := len(invs) + len(plan.Sources)
			if test.WantCreatedOrg {
				wantCreated++
			}
			if created != wantCreated {
				t.Errorf("Apply() created = %d, want %d", created, wantCreated)
			}
			for _, want := range test.WantWarns {
				if !slices.ContainsFunc(plan.Warnings, func(w string) bool {
					return strings.Contains(w, want)
				}) {
					t.Errorf("no warning contains %q:\n%s", want, strings.Join(plan.Warnings, "\n"))
				}
			}
		})
	}
}

// withUnplaced returns the plan's own account of where each inventory landed, with every inventory
// the want names and the plan places nowhere filled in as empty, so it compares with what is
// stored.
func withUnplaced(plan *importer.Plan, want map[string]string) map[string]string {
	out := plan.InventoryOrganizations()
	for name := range want {
		if _, ok := out[name]; !ok {
			out[name] = ""
		}
	}
	return out
}

// TestAWXSmartInventoryPlacementIsReported pins that a reader of the preview and of the assessment
// sees where a smart inventory lands, and that the organization is counted as coming across.
func TestAWXSmartInventoryPlacementIsReported(t *testing.T) {
	t.Parallel()
	plan, err := importer.FromAWX([]byte(placementExport), time.Date(2026, 9, 1, 0, 0, 0, 0,
		time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	report := plan.Report()
	if !slices.Contains(report.Created, importer.Count{Kind: "organizations", N: 1}) {
		t.Errorf("the summary does not count the organization: %+v", report.Created)
	}
	var doc bytes.Buffer
	importer.Render(&doc, "awx", "export.json", plan.Assess())
	if !strings.Contains(doc.String(), "all web (smart, in organization Ops)") {
		t.Errorf("the assessment does not say where the smart inventory lands:\n%s", doc.String())
	}
}

// listingLister resolves composed inventories in a test from the INI an import writes, reading
// each input's hosts with the package's own reader and listing them ungrouped, so a smart inventory
// can be previewed without Ansible.
type listingLister struct{}

// Run succeeds without running anything, since a preview never executes.
func (listingLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{}, nil
}

// ListInventory lists the hosts and variables of every source read together.
func (listingLister) ListInventory(_ context.Context, sources []string, _ string) ([]byte, error) {
	var names []string
	vars := map[string]map[string]any{}
	for _, src := range sources {
		body, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		hosts, err := inventory.Hosts(string(body))
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			names = append(names, h.Name)
			if len(h.Vars) > 0 {
				vars[h.Name] = h.Vars
			}
		}
	}
	return json.Marshal(map[string]any{
		"ungrouped": map[string]any{"hosts": names},
		"_meta":     map[string]any{"hostvars": vars},
	})
}

// TestImportedSmartInventoryReachesItsOrganizationOnly is the reach placement exists for: a smart
// inventory imported with its organization resolves to the hosts its organization's inventories
// hold and to none of another organization's, which is the set the same filter selects in AWX. The
// same filter with no organization reaches the other organization's host too, which is what the
// import produced before and what this guards against coming back.
func TestImportedSmartInventoryReachesItsOrganizationOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plan, err := importer.FromAWX([]byte(placementExport), time.Date(2026, 9, 1, 0, 0, 0, 0,
		time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	stores := placementStores(org.NewMemStore())
	if _, err := plan.Apply(ctx, stores); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	var smart *inventory.Inventory
	list, err := stores.Inventories.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, inv := range list {
		if inv.Kind == inventory.KindSmart {
			smart = inv
		}
	}
	if smart == nil {
		t.Fatal("the smart inventory was not stored")
	}
	d := dispatch.New(run.NewMemStore(), listingLister{}, zap.NewNop(),
		dispatch.WithInventories(stores.Inventories))
	t.Cleanup(d.Close)

	tests := []struct {
		Inv       *inventory.Inventory
		WantHosts []string
	}{{ // Test 0: Placed in its organization, it reads that organization's inventories only.
		Inv: smart, WantHosts: []string{"web1"},
	}, { // Test 1: With no organization it reads every inventory, reaching the other's host.
		Inv: &inventory.Inventory{ID: "inv_unplaced", Kind: smart.Kind,
			HostFilter: smart.HostFilter},
		WantHosts: []string{"web1", "web9"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			c, err := d.PreviewInventory(ctx, test.Inv)
			if err != nil {
				t.Fatalf("PreviewInventory() error = %v", err)
			}
			if diff := cmp.Diff(test.WantHosts, c.Resolution.Hosts); diff != "" {
				t.Errorf("resolved hosts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
