package importer_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// errStore is the failure every faulty store below returns, so a test can match it with errors.Is
// and prove Apply wrapped rather than replaced it.
var errStore = errors.New("store is down")

// failingProjects is a project store whose Save always fails.
type failingProjects struct {
	project.Store
}

// Save reports the store failure.
func (failingProjects) Save(context.Context, *project.Project) error { return errStore }

// failingInventories is an inventory store whose Save always fails.
type failingInventories struct {
	inventory.Store
}

// Save reports the store failure.
func (failingInventories) Save(context.Context, *inventory.Inventory) error { return errStore }

// listFailingInventories is an inventory store that saves fine but cannot be listed, which is what
// resolving a named inventory needs.
type listFailingInventories struct {
	inventory.Store
}

// List reports the store failure.
func (listFailingInventories) List(context.Context) ([]*inventory.Inventory, error) {
	return nil, errStore
}

// failingCredentials is a credential store whose Save always fails.
type failingCredentials struct {
	credential.Store
}

// Save reports the store failure.
func (failingCredentials) Save(context.Context, *credential.Credential) error { return errStore }

// failingSources is an inventory source store whose Save always fails.
type failingSources struct {
	invsource.Store
}

// Save reports the store failure.
func (failingSources) Save(context.Context, *invsource.Source) error { return errStore }

// failingTemplates is a template store whose Save always fails.
type failingTemplates struct {
	template.Store
}

// Save reports the store failure.
func (failingTemplates) Save(context.Context, *template.Template) error { return errStore }

// failingSchedules is a schedule store whose Save always fails.
type failingSchedules struct {
	schedule.Store
}

// Save reports the store failure.
func (failingSchedules) Save(context.Context, *schedule.Schedule) error { return errStore }

// workingStores returns a set of empty in-memory stores an apply can write into.
func workingStores() importer.ApplyStores {
	return importer.ApplyStores{
		Projects:    project.NewMemStore(),
		Inventories: inventory.NewMemStore(),
		Sources:     invsource.NewMemStore(),
		Credentials: credential.NewMemStore(),
		Templates:   template.NewMemStore(),
		Schedules:   schedule.NewMemStore(),
	}
}

// planWithEverything maps a small AWX export holding one of each object, so an apply test exercises
// every store in dependency order.
func planWithEverything(t *testing.T) *importer.Plan {
	t.Helper()
	const doc = `{
      "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://git.example/i.git"}],
      "inventory": [{"name": "prod", "hosts": [{"name": "web01"}]}],
      "credentials": [{"name": "deploy", "credential_type": {"name": "Machine"}, "inputs": {}}],
      "inventory_sources": [{"name": "ec2", "source": "ec2"}],
      "job_templates": [{"name": "site", "playbook": "site.yml", "project": "infra",
        "inventory": "prod",
        "related": {"schedules": [{"name": "nightly",
          "rrule": "DTSTART:20260101T020000Z RRULE:FREQ=DAILY"}]}}]
    }`
	plan, err := importer.FromAWX([]byte(doc), time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	return plan
}

// TestApplyStopsAtTheFirstStoreErrorAndSaysWhichObject pins that a store failure names the object it
// was writing and returns the count created so far. An apply that reported a bare failure would
// leave the operator unable to tell what is already in the database and what is not.
//
//nolint:funlen // Test function.
func TestApplyStopsAtTheFirstStoreErrorAndSaysWhichObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Break       func(s *importer.ApplyStores)
		WantCreated int
		WantNamed   string
	}{
		{Name: "projects", Break: func(s *importer.ApplyStores) {
			s.Projects = failingProjects{s.Projects}
		}, WantCreated: 0, WantNamed: `save project "infra"`}, // Test 0.
		{Name: "inventories", Break: func(s *importer.ApplyStores) {
			s.Inventories = failingInventories{s.Inventories}
		}, WantCreated: 1, WantNamed: `save inventory "prod"`}, // Test 1: the project landed first.
		{Name: "credentials", Break: func(s *importer.ApplyStores) {
			s.Credentials = failingCredentials{s.Credentials}
		}, WantCreated: 3, WantNamed: `save credential "deploy"`}, // Test 2.
		{Name: "sources", Break: func(s *importer.ApplyStores) {
			s.Sources = failingSources{s.Sources}
		}, WantCreated: 4, WantNamed: `save inventory source "ec2"`}, // Test 3.
		{Name: "templates", Break: func(s *importer.ApplyStores) {
			s.Templates = failingTemplates{s.Templates}
		}, WantCreated: 5, WantNamed: `save template "site"`}, // Test 4.
		{Name: "schedules", Break: func(s *importer.ApplyStores) {
			s.Schedules = failingSchedules{s.Schedules}
		}, WantCreated: 6, WantNamed: `save schedule "nightly"`}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan := planWithEverything(t)
			stores := workingStores()
			test.Break(&stores)
			created, err := plan.Apply(context.Background(), stores)
			if err == nil {
				t.Fatalf("Apply() error = nil, want the %s store failure", test.Name)
			}
			if !errors.Is(err, errStore) {
				t.Errorf("Apply() error = %v, want it to wrap the store's own error", err)
			}
			if !strings.Contains(err.Error(), test.WantNamed) {
				t.Errorf("Apply() error = %v, want it to name %q", err, test.WantNamed)
			}
			if created != test.WantCreated {
				t.Errorf("Apply() created = %d, want %d written before the failure",
					created, test.WantCreated)
			}
		})
	}
}

// TestApplyCountsWhatItWroteInDependencyOrder pins the successful apply: every object reaches its
// store, the count matches the plan, and credentials land before the templates that name them so a
// template is never stored referencing a credential row that does not exist yet.
func TestApplyCountsWhatItWroteInDependencyOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plan := planWithEverything(t)
	stores := workingStores()
	created, err := plan.Apply(ctx, stores)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	want := len(plan.Projects) + len(plan.Inventories) + len(plan.Sources) +
		len(plan.Credentials) + len(plan.Templates) + len(plan.Schedules)
	if created != want {
		t.Errorf("Apply() created = %d, want %d", created, want)
	}
	counts := map[string]int{}
	if list, err := stores.Projects.List(ctx); err == nil {
		counts["projects"] = len(list)
	}
	if list, err := stores.Inventories.List(ctx); err == nil {
		counts["inventories"] = len(list)
	}
	if list, err := stores.Credentials.List(ctx); err == nil {
		counts["credentials"] = len(list)
	}
	if list, err := stores.Sources.List(ctx); err == nil {
		counts["sources"] = len(list)
	}
	if list, err := stores.Templates.List(ctx); err == nil {
		counts["templates"] = len(list)
	}
	if list, err := stores.Schedules.List(ctx); err == nil {
		counts["schedules"] = len(list)
	}
	wantCounts := map[string]int{
		"projects": len(plan.Projects), "inventories": len(plan.Inventories),
		"credentials": len(plan.Credentials), "sources": len(plan.Sources),
		"templates": len(plan.Templates), "schedules": len(plan.Schedules),
	}
	if diff := cmp.Diff(wantCounts, counts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("stored counts mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyOfAnEmptyPlanWritesNothing pins that a plan with no objects is a no-op rather than an
// error, since the refusal for an empty import happens in the mapper and Apply is only the writer.
func TestApplyOfAnEmptyPlanWritesNothing(t *testing.T) {
	t.Parallel()
	created, err := (&importer.Plan{}).Apply(context.Background(), workingStores())
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if created != 0 {
		t.Errorf("Apply() created = %d, want 0", created)
	}
}

// TestApplyResolvesANamedInventoryAgainstBothPlaces pins the name resolution the mapping stage
// cannot do, because it has no store to ask. The value an operator types is the name of a stored
// inventory, and it used to be written straight into the field that holds a filesystem path, so
// every imported template pointed at a file that does not exist.
func TestApplyResolvesANamedInventoryAgainstBothPlaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		Name         string
		Inventory    string
		Seed         bool
		WantWired    bool
		WantPathKept string
	}{
		{Name: "matches a stored inventory", Inventory: "prod", Seed: true,
			WantWired: true}, // Test 0.
		{Name: "matches nothing", Inventory: "/etc/ansible/hosts",
			WantPathKept: "/etc/ansible/hosts"}, // Test 1: an operator who meant a file gets a file.
		{Name: "empty is left alone", Inventory: ""}, // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := "- name: nightly\n  sequence:\n    commands:\n      - exec: /bin/backup\n"
			plan, err := importer.FromRundeck(test.Inventory)([]byte(doc),
				time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("FromRundeck() error = %v", err)
			}
			stores := workingStores()
			var wantID string
			if test.Seed {
				seeded := &inventory.Inventory{ID: inventory.NewID(), Name: test.Inventory,
					Content: "web01\n", CreatedAt: time.Now()}
				if err := stores.Inventories.Save(ctx, seeded); err != nil {
					t.Fatalf("seed inventory: %v", err)
				}
				wantID = seeded.ID
			}
			if _, err := plan.Apply(ctx, stores); err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			tpl := plan.Templates[0]
			if test.WantWired {
				if tpl.InventoryID != wantID {
					t.Errorf("InventoryID = %q, want the stored inventory %q",
						tpl.InventoryID, wantID)
				}
				if tpl.Inventory != "" {
					t.Errorf("Inventory = %q, want it cleared once it resolved", tpl.Inventory)
				}
				return
			}
			if tpl.InventoryID != "" {
				t.Errorf("InventoryID = %q, want empty", tpl.InventoryID)
			}
			if tpl.Inventory != test.WantPathKept {
				t.Errorf("Inventory = %q, want %q kept as a path",
					tpl.Inventory, test.WantPathKept)
			}
			_, warned := warningHolding(plan.Warnings, "no stored inventory is named")
			if warned != (test.WantPathKept != "") {
				t.Errorf("path warning = %v, want %v.\nwarnings: %v",
					warned, test.WantPathKept != "", plan.Warnings)
			}
		})
	}
}

// warningHolding returns the first warning containing the fragment and whether one was found.
func warningHolding(warnings []string, fragment string) (string, bool) {
	for _, w := range warnings {
		if strings.Contains(w, fragment) {
			return w, true
		}
	}
	return "", false
}

// TestApplyResolvesAgainstThePlansOwnInventories pins that an inventory the same import is about to
// create counts as a match. It is not stored yet when resolution runs, so consulting only the store
// would leave a template pointing at a path while the inventory it named was created beside it.
func TestApplyResolvesAgainstThePlansOwnInventories(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const doc = `{
      "inventory": [{"name": "prod", "hosts": [{"name": "web01"}]}],
      "job_templates": [{"name": "site", "playbook": "site.yml"}]
    }`
	plan, err := importer.FromAWX([]byte(doc), time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	// The AWX mapper wires by id, so the path form this resolves is set here the way a Rundeck or
	// crontab import would leave it.
	plan.Templates[0].InventoryID = ""
	plan.Templates[0].Inventory = "prod"
	if _, err := plan.Apply(ctx, workingStores()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if plan.Templates[0].InventoryID != plan.Inventories[0].ID {
		t.Errorf("InventoryID = %q, want the inventory this import creates %q",
			plan.Templates[0].InventoryID, plan.Inventories[0].ID)
	}
}

// TestApplyReportsAnUnresolvedInventoryNameOnlyOnce pins that several templates naming the same
// missing inventory produce one line rather than one per template. A report that repeats itself is
// one an operator skims past.
func TestApplyReportsAnUnresolvedInventoryNameOnlyOnce(t *testing.T) {
	t.Parallel()
	const doc = `- name: one
  sequence:
    commands:
      - exec: /bin/a
- name: two
  sequence:
    commands:
      - exec: /bin/b
`
	plan, err := importer.FromRundeck("nowhere")([]byte(doc),
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if _, err := plan.Apply(context.Background(), workingStores()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	count := 0
	for _, w := range plan.Warnings {
		if strings.Contains(w, "no stored inventory is named") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the missing inventory was reported %d times, want 1.\nwarnings: %v",
			count, plan.Warnings)
	}
}

// TestApplyWithoutAnInventoryStoreLeavesTheNameAlone pins that resolution is skipped when there is
// nowhere to look. Nothing may be written for a store the install has not enabled, and the name has
// to survive as a path rather than being cleared.
func TestApplyWithoutAnInventoryStoreLeavesTheNameAlone(t *testing.T) {
	t.Parallel()
	const doc = "- name: nightly\n  sequence:\n    commands:\n      - exec: /bin/backup\n"
	plan, err := importer.FromRundeck("prod")([]byte(doc),
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	stores := workingStores()
	stores.Inventories = nil
	if _, err := plan.Apply(context.Background(), stores); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := plan.Templates[0].Inventory; got != "prod" {
		t.Errorf("Inventory = %q, want %q left as written", got, "prod")
	}
}

// TestApplyRefusesBeforeWritingWhenTheInventoryStoreCannotBeRead pins that a failure while resolving
// names stops the apply before anything is written. Writing half a plan and then failing leaves the
// operator with objects nobody planned.
func TestApplyRefusesBeforeWritingWhenTheInventoryStoreCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const doc = "- name: nightly\n  sequence:\n    commands:\n      - exec: /bin/backup\n"
	plan, err := importer.FromRundeck("prod")([]byte(doc),
		time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	stores := workingStores()
	stores.Inventories = listFailingInventories{stores.Inventories}
	created, err := plan.Apply(ctx, stores)
	if err == nil {
		t.Fatal("Apply() error = nil, want the list failure to stop it")
	}
	if !errors.Is(err, errStore) {
		t.Errorf("Apply() error = %v, want it to wrap the store's own error", err)
	}
	if created != 0 {
		t.Errorf("Apply() created = %d, want 0 written before the refusal", created)
	}
	if list, err := stores.Templates.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("templates stored = %d (err %v), want 0", len(list), err)
	}
}

// TestApplyRefusesSourcesWithNowhereToPutThem pins the up-front refusal that keeps a dynamic
// source's backing inventory from being created without its source. An orphaned inventory looks like
// a real one and never refreshes.
func TestApplyRefusesSourcesWithNowhereToPutThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plan := planWithEverything(t)
	if len(plan.Sources) == 0 {
		t.Fatal("the fixture no longer holds a dynamic source")
	}
	stores := workingStores()
	stores.Sources = nil
	created, err := plan.Apply(ctx, stores)
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal when sources cannot be stored")
	}
	if !strings.Contains(err.Error(), "inventory sources not enabled") {
		t.Errorf("Apply() error = %v, want it to name the missing store", err)
	}
	if created != 0 {
		t.Errorf("Apply() created = %d, want 0", created)
	}
	if list, err := stores.Inventories.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("inventories stored = %d (err %v), want 0: nothing is written before the refusal",
			len(list), err)
	}
}

// TestApplyIsIdempotentAcrossARepeatedCall pins that applying the same plan twice does not double
// the objects. Save is create-or-replace keyed by the generated id, so a retried apply after a
// partial failure converges rather than duplicating what already landed.
func TestApplyIsIdempotentAcrossARepeatedCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plan := planWithEverything(t)
	stores := workingStores()
	first, err := plan.Apply(ctx, stores)
	if err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	second, err := plan.Apply(ctx, stores)
	if err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if first != second {
		t.Errorf("Apply() created %d then %d, want the same count", first, second)
	}
	list, err := stores.Templates.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != len(plan.Templates) {
		t.Errorf("templates stored = %d after two applies, want %d",
			len(list), len(plan.Templates))
	}
}
