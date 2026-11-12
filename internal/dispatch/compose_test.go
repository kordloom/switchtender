package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// composeFixture stores three inventories and returns a dispatcher over them that resolves through
// a ListingRunner, so no Ansible is needed.
//
// inv_a and inv_b belong to org_x and inv_c to org_y. web1 is in both inv_a and inv_b with
// different variables, which is what proves a smart inventory takes each host name from the first
// input.
func composeFixture(t *testing.T, opts ...Option) (*Dispatcher, inventory.Store, run.Store,
	*inventorytest.ListingRunner) {
	t.Helper()
	invs := composeInventories(t)
	runner := &inventorytest.ListingRunner{}
	store := run.NewMemStore()
	d := New(store, runner, zap.NewNop(), append([]Option{WithInventories(invs)}, opts...)...)
	t.Cleanup(d.Close)
	return d, invs, store, runner
}

// composeInventories returns the input inventories every composition test draws on: two in one
// organization and one in another.
func composeInventories(t *testing.T) inventory.Store {
	t.Helper()
	ctx := context.Background()
	invs := inventory.NewMemStore()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for n, inv := range []*inventory.Inventory{{
		ID: "inv_a", Name: "production", OrgID: "org_x",
		Content: inventorytest.Listing(map[string][]string{"web": {"web1", "web2"}, "prod": {"web1"}},
			map[string]map[string]any{"web1": {"env": "prod"}, "web2": {"env": "dev"}}),
	}, {
		ID: "inv_b", Name: "databases", OrgID: "org_x",
		Content: inventorytest.Listing(map[string][]string{"db": {"db1"}, "web": {"web1"}},
			map[string]map[string]any{"db1": {"env": "prod"}, "web1": {"env": "shadow"}}),
	}, {
		ID: "inv_c", Name: "other tenant", OrgID: "org_y",
		Content: inventorytest.Listing(map[string][]string{"web": {"web9"}}, nil),
	}} {
		inv.CreatedAt = base.Add(time.Duration(n) * time.Minute)
		if err := invs.Save(ctx, inv); err != nil {
			t.Fatalf("Save(%s) error = %v", inv.ID, err)
		}
	}
	return invs
}

// denyInventory is an access check that refuses one inventory id and allows every other.
func denyInventory(id string) run.InventoryAccessFunc {
	return func(_ context.Context, got string) (bool, error) { return got != id, nil }
}

// TestComposedResolution pins what a smart and a constructed inventory resolve to, and that an
// actor who may not use an input never gets its hosts. Composing must not widen access: a person
// allowed one composed inventory reaches exactly the hosts they could have targeted by naming its
// inputs.
func TestComposedResolution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Inv        *inventory.Inventory
		Access     run.InventoryAccessFunc
		WantHosts  []string
		WantInputs []string
		Want       error
	}{{ // Test 0: A smart inventory reads every inventory in its organization, first input winning.
		Inv: &inventory.Inventory{ID: "inv_s", Kind: inventory.KindSmart, OrgID: "org_x",
			HostFilter: "groups__name=web or groups__name=db"},
		WantHosts: []string{"db1", "web1", "web2"}, WantInputs: []string{"inv_a", "inv_b"},
	}, { // Test 1: A host name in two inputs is taken from the first whose record matches, as AWX
		// keeps one host per name among the matches.
		Inv: &inventory.Inventory{ID: "inv_s", Kind: inventory.KindSmart, OrgID: "org_x",
			HostFilter: "variables__env=shadow"},
		WantHosts: []string{"web1"}, WantInputs: []string{"inv_b"},
	}, { // Test 2: An unowned smart inventory reads every inventory, across organizations.
		Inv: &inventory.Inventory{ID: "inv_s", Kind: inventory.KindSmart,
			HostFilter: "groups__name=web"},
		WantHosts: []string{"web1", "web2", "web9"}, WantInputs: []string{"inv_a", "inv_c"},
	}, { // Test 3: An input the actor may not use contributes nothing to a smart inventory.
		Inv: &inventory.Inventory{ID: "inv_s", Kind: inventory.KindSmart, OrgID: "org_x",
			HostFilter: "groups__name=web or groups__name=db"},
		Access:    denyInventory("inv_a"),
		WantHosts: []string{"db1", "web1"}, WantInputs: []string{"inv_b"},
	}, { // Test 4: A constructed inventory merges its inputs in order.
		Inv: &inventory.Inventory{ID: "inv_k", Kind: inventory.KindConstructed,
			InputIDs: []string{"inv_a", "inv_c"}},
		WantHosts: []string{"web1", "web2", "web9"}, WantInputs: []string{"inv_a", "inv_c"},
	}, { // Test 5: An input the actor may not use contributes nothing to a constructed inventory.
		Inv: &inventory.Inventory{ID: "inv_k", Kind: inventory.KindConstructed,
			InputIDs: []string{"inv_a", "inv_c"}},
		Access:    denyInventory("inv_c"),
		WantHosts: []string{"web1", "web2"}, WantInputs: []string{"inv_a"},
	}, { // Test 6: A constructed inventory whose input is gone is refused.
		Inv: &inventory.Inventory{ID: "inv_k", Kind: inventory.KindConstructed,
			InputIDs: []string{"inv_gone"}},
		Want: inventory.ErrComposition,
	}, { // Test 7: A smart inventory with a filter that does not parse is refused.
		Inv:  &inventory.Inventory{ID: "inv_s", Kind: inventory.KindSmart, HostFilter: "hostname=x"},
		Want: inventory.ErrHostFilter,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := composeFixture(t)
			ctx := run.WithInventoryAccess(context.Background(), test.Access)
			got, err := d.PreviewInventory(ctx, test.Inv)
			if !errors.Is(err, test.Want) {
				t.Fatalf("PreviewInventory() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			res := got.Resolution
			if diff := cmp.Diff(test.WantHosts, res.Hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hosts mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantInputs, res.Inputs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("inputs mismatch (-want +got):\n%s", diff)
			}
			for _, h := range got.Resolution.Hosts {
				if !strings.Contains(got.Content, `"`+h+`"`) {
					t.Errorf("the rendered inventory lacks %s, which it resolved to", h)
				}
			}
		})
	}
}

// TestComposedSmartReadsFacts pins that ansible_facts terms read the facts fleet memory gathered,
// and that a host never gathered does not match.
func TestComposedSmartReadsFacts(t *testing.T) {
	t.Parallel()
	d, _, store, _ := composeFixture(t)
	ctx := context.Background()
	gathered := time.Now().Add(-time.Hour)
	if err := store.Save(ctx, &run.Run{ID: "run_facts", Playbook: "gather.yml",
		Status: run.StatusSucceeded, CreatedAt: gathered}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveHostFacts(ctx, "run_facts", []run.HostFacts{
		{Host: "web2", Facts: map[string]string{"distribution": "Ubuntu"}, GatheredAt: gathered},
		{Host: "db1", Facts: map[string]string{"distribution": "RedHat"}, GatheredAt: gathered},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	got, err := d.PreviewInventory(ctx, &inventory.Inventory{
		ID: "inv_s", Kind: inventory.KindSmart, HostFilter: "ansible_facts__ansible_distribution=Ubuntu",
	})
	if err != nil {
		t.Fatalf("PreviewInventory() error = %v", err)
	}
	if diff := cmp.Diff([]string{"web2"}, got.Resolution.Hosts); diff != "" {
		t.Errorf("hosts mismatch (-want +got):\n%s", diff)
	}
}

// TestComposedRunRecordsAndIsHeldToItsHosts drives a run against a smart inventory through submit
// and execution. The run records the hosts the inventory resolved to for the launching actor, the
// spec the chain commits carries them, and execution is held to them: a host added to an input
// after the launch is not reached, and an input the actor could not use is not read.
func TestComposedRunRecordsAndIsHeldToItsHosts(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	d, invs, store, runner := composeFixture(t, WithClaimGate(func() error {
		select {
		case <-gate:
			return nil
		default:
			return errors.New("held until the inputs change")
		}
	}))
	ctx := context.Background()
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_smart", Name: "web everywhere", Kind: inventory.KindSmart, OrgID: "org_x",
		HostFilter: "groups__name=web or groups__name=db",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	launch := run.WithInventoryAccess(ctx, denyInventory("inv_b"))
	created, err := d.Submit(launch, "site.yml", "", run.WithInventory("inv_smart"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	want := &run.InventoryResolution{
		Kind: "smart", Inputs: []string{"inv_a"}, Hosts: []string{"web1", "web2"},
		Engine: inventory.EngineNative,
	}
	digests := cmpopts.IgnoreFields(run.InventoryResolution{}, "InputDigest", "ResolvedDigest")
	if diff := cmp.Diff(want, created.InventoryResolution, digests); diff != "" {
		t.Fatalf("recorded resolution mismatch (-want +got):\n%s", diff)
	}
	for name, d := range map[string]string{"input": created.InventoryResolution.InputDigest,
		"resolved": created.InventoryResolution.ResolvedDigest} {
		if !strings.HasPrefix(d, "sha256:") {
			t.Errorf("the %s digest %q is not recorded", name, d)
		}
	}

	// The evidence: the spec the decision and outcome entries commit names the hosts.
	spec, err := outcome.Spec(created)
	if err != nil {
		t.Fatalf("outcome.Spec() error = %v", err)
	}
	if !strings.Contains(string(spec), `"hosts":["web1","web2"]`) {
		t.Errorf("the committed spec does not name the resolved hosts: %s", spec)
	}
	bare := created.Clone()
	bare.InventoryResolution = nil
	withSet, err := outcome.SpecDigest(created)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	without, err := outcome.SpecDigest(bare)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	if withSet == without {
		t.Error("the spec digest does not cover the resolved hosts, so the chain cannot show them")
	}

	// A host joins an input after the launch. The run must not reach it.
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_a", Name: "production", OrgID: "org_x",
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Content:   inventorytest.Listing(map[string][]string{"web": {"web1", "web2", "web3"}}, nil),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	close(gate)
	d.wake()
	done := dispatchtest.WaitTerminal(t, store, created.ID)
	if done.Status != run.StatusSucceeded {
		t.Fatalf("run status = %q (%s), want succeeded", done.Status, done.Error)
	}
	if diff := cmp.Diff(created.InventoryResolution, done.InventoryResolution); diff != "" {
		t.Errorf("stored resolution mismatch (-want +got):\n%s", diff)
	}
	executed := runner.Executed()
	if len(executed) != 1 {
		t.Fatalf("runner ran %d times, want 1", len(executed))
	}
	for _, host := range []string{"web1", "web2"} {
		if !strings.Contains(executed[0], host) {
			t.Errorf("the executed inventory lacks %s, which the run resolved to:\n%s", host,
				executed[0])
		}
	}
	for _, host := range []string{"web3", "db1"} {
		if strings.Contains(executed[0], host) {
			t.Errorf("the executed inventory reaches %s, which the run never resolved to:\n%s", host,
				executed[0])
		}
	}
}

// TestComposedRunMatchesInputScopedPolicy pins that a rule scoped to an inventory governs a run
// that reaches its hosts through a smart inventory. Otherwise a smart inventory over production
// hosts would walk around every approval rule written for production.
func TestComposedRunMatchesInputScopedPolicy(t *testing.T) {
	t.Parallel()
	policies := policy.NewMemStore()
	ctx := context.Background()
	if err := policies.Save(ctx, &policy.Policy{
		ID: "pol_prod", Name: "production needs a second person", InventoryID: "inv_a",
		Effect: policy.EffectRequireApproval, MaxDestroy: -1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d, invs, _, _ := composeFixture(t, WithPolicies(policies))
	for _, inv := range []*inventory.Inventory{
		{ID: "inv_dbonly", Name: "databases only", Kind: inventory.KindSmart, OrgID: "org_x",
			HostFilter: "groups__name=db"},
		{ID: "inv_webs", Name: "webs", Kind: inventory.KindSmart, OrgID: "org_x",
			HostFilter: "name=web2"},
	} {
		if err := invs.Save(ctx, inv); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	tests := []struct {
		InventoryID string
		WantStatus  run.Status
	}{
		{InventoryID: "inv_webs", WantStatus: run.StatusPendingApproval}, // Test 0: Draws from inv_a.
		{InventoryID: "inv_dbonly", WantStatus: run.StatusPending},       // Test 1: Never touches inv_a.
	}
	for testNum, test := range tests {
		created, err := d.Submit(ctx, "site.yml", "", run.WithInventory(test.InventoryID))
		if err != nil {
			t.Fatalf("test %d: Submit() error = %v", testNum, err)
		}
		if created.Status != test.WantStatus {
			t.Errorf("test %d: %s submitted %q, want %q", testNum, test.InventoryID, created.Status,
				test.WantStatus)
		}
	}
}

// TestComposedRunRefusesNoHosts pins that a launch whose composed inventory resolves to nothing is
// refused, rather than started against no host and reported as having run.
func TestComposedRunRefusesNoHosts(t *testing.T) {
	t.Parallel()
	d, invs, _, _ := composeFixture(t)
	ctx := context.Background()
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_none", Name: "nothing", Kind: inventory.KindSmart, HostFilter: "name=absent",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	_, err := d.Submit(ctx, "site.yml", "", run.WithInventory("inv_none"))
	if !errors.Is(err, inventory.ErrNoHosts) {
		t.Errorf("Submit() error = %v, want ErrNoHosts", err)
	}
}

// TestComposedRunWithoutResolutionIsRefused pins the fail-closed side of execution: a composed run
// that reaches an executor with no recorded resolution is not resolved there, where no actor is
// known, because that would reach every host on the install.
func TestComposedRunWithoutResolutionIsRefused(t *testing.T) {
	t.Parallel()
	d, invs, _, _ := composeFixture(t)
	ctx := context.Background()
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_smart", Name: "webs", Kind: inventory.KindSmart, HostFilter: "groups__name=web",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	_, cleanup, _, _, err := d.inventoryFile(ctx, "inv_smart", nil)
	cleanup()
	if !errors.Is(err, inventory.ErrResolve) {
		t.Errorf("inventoryFile() with no resolution error = %v, want ErrResolve", err)
	}
}

// requireAnsibleInventory skips a test that needs ansible-inventory when it is absent, unless the
// suite is required to run in full, which is how CI says the binary must be there.
func requireAnsibleInventory(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ansible-inventory"); err != nil {
		if strings.TrimSpace(os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE")) != "" {
			t.Fatalf("ansible-inventory is required: %v", err)
		}
		t.Skip("ansible-inventory not installed")
	}
}

// TestConstructedWithAnsible resolves a constructed inventory through real ansible-inventory: INI
// and YAML inputs, compose, groups, and keyed_groups, and a limit naming a constructed group. This
// is the subset AWX documents for constructed inventories, evaluated by the same plugin AWX runs.
func TestConstructedWithAnsible(t *testing.T) {
	t.Parallel()
	requireAnsibleInventory(t)
	ctx := context.Background()
	invs := inventory.NewMemStore()
	for _, inv := range []*inventory.Inventory{{
		ID: "inv_ini", Name: "ini fleet",
		Content: "[web]\nweb1 env=prod\nweb2 env=dev\n\n[db]\ndb1 state=shutdown env=prod\n\n" +
			"[prod:children]\nweb\n\n[prod:vars]\ntier=gold\n",
	}, {
		ID: "inv_yaml", Name: "yaml fleet",
		Content: "all:\n  hosts:\n    lb1:\n      state: shutdown\n      account_alias: product_dev\n" +
			"    lb2:\n      account_alias: product_dev\n",
	}} {
		if err := invs.Save(ctx, inv); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	d := New(run.NewMemStore(), roundhouse.NewAnsibleRunner(), zap.NewNop(), WithInventories(invs))
	t.Cleanup(d.Close)
	if version := realAnsibleCore(t); !atLeastRelease(version, constructedFloor) {
		_, err := d.PreviewInventory(ctx, &inventory.Inventory{
			ID: "inv_k", Kind: inventory.KindConstructed, InputIDs: []string{"inv_ini", "inv_yaml"},
		})
		if !errors.Is(err, inventory.ErrResolve) || !strings.Contains(err.Error(), version) {
			t.Fatalf("PreviewInventory() on ansible-core %s error = %v, want the floor refusal",
				version, err)
		}
		return
	}
	options := "strict: false\n" +
		"compose:\n  resolved_state: state | default('running')\n" +
		"groups:\n  is_shutdown: resolved_state == 'shutdown'\n" +
		"keyed_groups:\n  - key: account_alias\n    prefix: acct\n"
	tests := []struct {
		Limit      string
		WantHosts  []string
		WantGroups []string
	}{{ // Test 0: No limit keeps every host and builds the groups.
		Limit:      "",
		WantHosts:  []string{"db1", "lb1", "lb2", "web1", "web2"},
		WantGroups: []string{"acct_product_dev", "is_shutdown"},
	}, { // Test 1: A limit may name a constructed group.
		Limit: "is_shutdown", WantHosts: []string{"db1", "lb1"}, WantGroups: []string{"is_shutdown"},
	}, { // Test 2: A limit may intersect two of them.
		Limit: "is_shutdown:&acct_product_dev", WantHosts: []string{"lb1"},
		WantGroups: []string{"acct_product_dev"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := d.PreviewInventory(ctx, &inventory.Inventory{
				ID: "inv_k", Kind: inventory.KindConstructed, InputIDs: []string{"inv_ini", "inv_yaml"},
				SourceVars: options, Limit: test.Limit,
			})
			if err != nil {
				t.Fatalf("PreviewInventory() error = %v", err)
			}
			if diff := cmp.Diff(test.WantHosts, got.Resolution.Hosts); diff != "" {
				t.Errorf("hosts mismatch (-want +got):\n%s", diff)
			}
			for _, g := range test.WantGroups {
				if !strings.Contains(got.Content, `"`+g+`"`) {
					t.Errorf("the rendered inventory lacks the constructed group %s:\n%s", g,
						got.Content)
				}
			}
			if !strings.Contains(got.Content, "resolved_state") {
				t.Errorf("the composed variable did not reach the rendered inventory:\n%s",
					got.Content)
			}
		})
	}
}

// TestSmartWithAnsible resolves a smart inventory through real ansible-inventory, so the groups and
// the group variables a filter reads are the ones Ansible itself parses out of an INI inventory.
func TestSmartWithAnsible(t *testing.T) {
	t.Parallel()
	requireAnsibleInventory(t)
	ctx := context.Background()
	invs := inventory.NewMemStore()
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_ini", Name: "ini fleet",
		Content: "[web]\nweb1\nweb2\n\n[db]\ndb1\n\n[web:vars]\ntier=gold\n",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(run.NewMemStore(), roundhouse.NewAnsibleRunner(), zap.NewNop(), WithInventories(invs))
	t.Cleanup(d.Close)
	got, err := d.PreviewInventory(ctx, &inventory.Inventory{
		ID: "inv_s", Kind: inventory.KindSmart, HostFilter: "variables__tier=gold and not name=web2",
	})
	if err != nil {
		t.Fatalf("PreviewInventory() error = %v", err)
	}
	if diff := cmp.Diff([]string{"web1"}, got.Resolution.Hosts); diff != "" {
		t.Errorf("hosts mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(got.Content, "gold") {
		t.Errorf("the host's group variable did not reach the rendered inventory:\n%s", got.Content)
	}
}

// realAnsibleCore returns the version of the ansible-core installed here.
func realAnsibleCore(t *testing.T) string {
	t.Helper()
	d := New(run.NewMemStore(), roundhouse.NewAnsibleRunner(), zap.NewNop())
	t.Cleanup(d.Close)
	version, err := d.AnsibleCore(context.Background())
	if err != nil {
		t.Fatalf("AnsibleCoreVersion() error = %v", err)
	}
	return version
}

// taintedInput is an input whose host variable holds a lookup, with a value no message may quote.
const taintedInput = "all:\n  hosts:\n    h1:\n      v: \"{{ lookup('pipe', 'SECRETCMD') }}\"\n"

// hostListingRunner is a ListingRunner that can also list a split's hosts, counting each call.
type hostListingRunner struct {
	*inventorytest.ListingRunner
	// hostCalls counts the host lists asked for.
	hostCalls atomic.Int32
}

// Hosts counts the call and returns two hosts.
func (r *hostListingRunner) Hosts(context.Context, string, string) ([]string, error) {
	r.hostCalls.Add(1)
	return []string{"h1", "web1"}, nil
}

// TestConstructedInputIsRefusedBeforeAnyProcess pins that an input holding template text is
// refused before ansible-inventory or the version probe runs, at every entry that composes, and
// that the refusal names the constructed inventory and the input and quotes no value. The runner
// reports Ansible as missing, so a refusal that came after the probe would say Ansible is needed.
func TestConstructedInputIsRefusedBeforeAnyProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Entry string
	}{{ // Test 0: The preview.
		Entry: "preview",
	}, { // Test 1: A launch.
		Entry: "submit",
	}, { // Test 2: A split.
		Entry: "split",
	}, { // Test 3: The retry of a split's shards, held to a recorded resolution.
		Entry: "pinned",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			invs := inventory.NewMemStore()
			k := &inventory.Inventory{ID: "inv_k", Name: "fleet view", Kind: inventory.KindConstructed,
				InputIDs: []string{"inv_clean", "inv_tainted"}}
			for _, inv := range []*inventory.Inventory{
				{ID: "inv_clean", Name: "clean", Content: "[web]\nweb1 tier=gold\n"},
				{ID: "inv_tainted", Name: "tainted", Content: taintedInput}, k,
			} {
				if err := invs.Save(ctx, inv); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			runner := &hostListingRunner{ListingRunner: &inventorytest.ListingRunner{Missing: true}}
			store := run.NewMemStore()
			d := New(store, runner, zap.NewNop(), WithInventories(invs))
			t.Cleanup(d.Close)
			var err error
			switch test.Entry {
			case "preview":
				_, err = d.PreviewInventory(ctx, k)
			case "submit":
				_, err = d.Submit(ctx, "site.yml", "", run.WithInventory(k.ID))
			case "split":
				_, err = d.SubmitSplit(ctx, "site.yml", "", 2, run.WithInventory(k.ID))
			case "pinned":
				_, err = d.composedContent(ctx, k, &run.InventoryResolution{Hosts: []string{"h1"},
					Inputs: []string{"inv_clean", "inv_tainted"}})
			}
			if !errors.Is(err, inventory.ErrResolve) {
				t.Fatalf("%s error = %v, want ErrResolve", test.Entry, err)
			}
			if errors.Is(err, inventory.ErrNeedsAnsible) {
				t.Errorf("%s error = %v, which says Ansible is needed", test.Entry, err)
			}
			for _, want := range []string{`"fleet view"`, `"tainted"`, `variable v of host "h1"`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s error = %q, want it to name %s", test.Entry, err, want)
				}
			}
			if strings.Contains(err.Error(), "SECRETCMD") {
				t.Errorf("%s error = %q, which quotes the value", test.Entry, err)
			}
			if runner.Lists() != 0 || runner.Reads() != 0 || runner.hostCalls.Load() != 0 {
				t.Errorf("%s ran %d listings, %d reads, and %d host lists, want none", test.Entry,
					runner.Lists(), runner.Reads(), runner.hostCalls.Load())
			}
			if runs, lerr := store.List(ctx); lerr != nil || len(runs) != 0 {
				t.Errorf("%s stored %d runs (error %v), want none", test.Entry, len(runs), lerr)
			}
		})
	}
}

// TestConstructedNeedsTheFloorRelease pins that a constructed inventory is composed only with
// ansible-core 2.19 or later, and that the refusal comes before any listing and names both the
// installed version and the floor.
func TestConstructedNeedsTheFloorRelease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Version string
		Want    error
	}{{ // Test 0: 2.16 is refused.
		Version: "2.16.19", Want: inventory.ErrResolve,
	}, { // Test 1: 2.17 is refused.
		Version: "2.17.14", Want: inventory.ErrResolve,
	}, { // Test 2: 2.18 is refused.
		Version: "2.18.19", Want: inventory.ErrResolve,
	}, { // Test 3: A version that cannot be read counts as older.
		Version: "devel", Want: inventory.ErrResolve,
	}, { // Test 4: A minor that is not a number counts as older.
		Version: "2.x.1", Want: inventory.ErrResolve,
	}, { // Test 5: 2.19 composes.
		Version: "2.19.13",
	}, { // Test 6: A later major composes.
		Version: "3.0.0",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d, _, _, runner := composeFixture(t)
			runner.Version = test.Version
			_, err := d.PreviewInventory(context.Background(), &inventory.Inventory{
				ID: "inv_k", Name: "fleet view", Kind: inventory.KindConstructed,
				InputIDs: []string{"inv_a"},
			})
			if !errors.Is(err, test.Want) {
				t.Fatalf("PreviewInventory() error = %v, want %v", err, test.Want)
			}
			if err == nil {
				return
			}
			for _, want := range []string{`"` + test.Version + `"`, constructedFloor} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("PreviewInventory() error = %q, want it to name %s", err, want)
				}
			}
			if runner.Lists() != 0 {
				t.Errorf("PreviewInventory() ran %d listings, want none", runner.Lists())
			}
		})
	}
}

// TestConstructedInputNeverRunsOnTheServer runs real ansible-inventory over two inputs that try to
// run a command on the server: one with a lookup written out, refused by the scan, and one with
// none, whose braces are escapes a definition decodes with from_yaml. Neither command runs. On a
// release below the floor nothing is listed at all.
func TestConstructedInputNeverRunsOnTheServer(t *testing.T) {
	t.Parallel()
	requireAnsibleInventory(t)
	tests := []struct {
		Input      func(marker string) string
		SourceVars string
		Want       error
	}{{ // Test 0: A lookup written out in an input is refused.
		Input: func(marker string) string {
			return "all:\n  hosts:\n    h1:\n      v: \"{{ lookup('pipe', 'touch " + marker + "') }}\"\n"
		},
		SourceVars: "compose:\n  w: v\n",
		Want:       inventory.ErrResolve,
	}, { // Test 1: A lookup with no marker, decoded by the definition, does not run.
		Input: func(marker string) string {
			return "all:\n  hosts:\n    h1:\n      y: 'k: \"\\x7b\\x7b lookup(''pipe'', ''touch " +
				marker + "'') \\x7d\\x7d\"'\n"
		},
		SourceVars: "strict: false\ncompose:\n  hn: (y | from_yaml).k\ngroups:\n  ran: \"'x' in hn\"\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			marker := filepath.Join(t.TempDir(), "ran")
			invs := inventory.NewMemStore()
			if err := invs.Save(ctx, &inventory.Inventory{ID: "inv_in", Name: "input",
				Content: test.Input(marker)}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			d := New(run.NewMemStore(), roundhouse.NewAnsibleRunner(), zap.NewNop(),
				WithInventories(invs))
			t.Cleanup(d.Close)
			want := test.Want
			if !atLeastRelease(realAnsibleCore(t), constructedFloor) {
				want = inventory.ErrResolve
			}
			_, err := d.PreviewInventory(ctx, &inventory.Inventory{ID: "inv_k", Name: "view",
				Kind: inventory.KindConstructed, InputIDs: []string{"inv_in"},
				SourceVars: test.SourceVars})
			if !errors.Is(err, want) {
				t.Fatalf("PreviewInventory() error = %v, want %v", err, want)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Errorf("the command in the input ran on the server and made %s", marker)
			}
		})
	}
}

// TestConstructedHandsThePlayPlainStrings composes with real ansible-inventory and checks the
// inventory a run is given: an input's strings come back as plain strings at every depth, a
// composed value that holds template text keeps the form Ansible will not template, and a play over
// that inventory reads the plain values.
func TestConstructedHandsThePlayPlainStrings(t *testing.T) {
	t.Parallel()
	requireAnsibleInventory(t)
	if version := realAnsibleCore(t); !atLeastRelease(version, constructedFloor) {
		t.Skipf("ansible-core %s is below the constructed floor, which another test pins", version)
	}
	ctx := context.Background()
	invs := inventory.NewMemStore()
	if err := invs.Save(ctx, &inventory.Inventory{ID: "inv_in", Name: "input",
		Content: "all:\n  hosts:\n    h1:\n      ansible_connection: local\n      tier: gold\n" +
			"      nested:\n        list: [a, {b: c}]\n"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(run.NewMemStore(), roundhouse.NewAnsibleRunner(), zap.NewNop(), WithInventories(invs))
	t.Cleanup(d.Close)
	got, err := d.PreviewInventory(ctx, &inventory.Inventory{ID: "inv_k", Name: "view",
		Kind: inventory.KindConstructed, InputIDs: []string{"inv_in"},
		SourceVars: "compose:\n  marked: \"'{' ~ '{ x }' ~ '}'\"\n"})
	if err != nil {
		t.Fatalf("PreviewInventory() error = %v", err)
	}
	for _, want := range []string{`"tier": "gold"`, `"a"`, `"b": "c"`} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("the run's inventory lacks %s, a plain string:\n%s", want, got.Content)
		}
	}
	if !strings.Contains(got.Content, `"__ansible_unsafe": "{{ x }}"`) {
		t.Errorf("the composed template text lost its unsafe form:\n%s", got.Content)
	}
	if strings.Count(got.Content, "__ansible_unsafe") != 1 {
		t.Errorf("the run's inventory holds unsafe objects besides the composed one:\n%s", got.Content)
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed")
	}
	dir := t.TempDir()
	invPath := filepath.Join(dir, "inventory.json")
	playPath := filepath.Join(dir, "play.yml")
	play := "- hosts: all\n  gather_facts: false\n  tasks:\n" +
		"    - ansible.builtin.assert:\n        that:\n" +
		"          - tier == 'gold'\n          - nested.list[0] == 'a'\n" +
		"          - nested.list[1].b == 'c'\n          - marked | length == 7\n" +
		"    - ansible.builtin.debug:\n        msg: tier read\n      when: tier == 'gold'\n"
	if err := os.WriteFile(invPath, []byte(got.Content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(playPath, []byte(play), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "ansible-playbook", "-i", invPath, playPath)
	cmd.Env = append(os.Environ(), "ANSIBLE_HOME="+dir, "ANSIBLE_LOCAL_TEMP="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-playbook error = %v:\n%s", err, out)
	}
	if !strings.Contains(string(out), "tier read") {
		t.Errorf("the task reading the inventory string was skipped:\n%s", out)
	}
}
