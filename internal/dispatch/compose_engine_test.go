package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// pluginConfig is an input only Ansible can read: a dynamic inventory plugin's configuration.
const pluginConfig = "plugin: amazon.aws.aws_ec2\nregions:\n  - us-east-1\n"

// pluginListing is what ansible-inventory would print for pluginConfig in these tests.
const pluginListing = `{"_meta": {"hostvars": {"ec2-1": {"region": "us-east-1"}}},` +
	`"all": {"children": ["aws_ec2"]}, "aws_ec2": {"hosts": ["ec2-1"]}}`

// TestComposedEngineFollowsTheDefinition pins that what an inventory is decides which engine
// resolves it, never whether Ansible is installed: a smart inventory over static documents is
// native with or without Ansible, an input only Ansible reads sends that input to Ansible, and a
// constructed inventory is always Ansible's. When Ansible is needed and missing, the error says why
// and gives the one-line install.
func TestComposedEngineFollowsTheDefinition(t *testing.T) {
	t.Parallel()
	smart := &inventory.Inventory{ID: "inv_s", Name: "webs", Kind: inventory.KindSmart,
		HostFilter: "name__startswith=web or name__startswith=ec2"}
	constructed := &inventory.Inventory{ID: "inv_k", Name: "built", Kind: inventory.KindConstructed,
		InputIDs: []string{"inv_static"}}
	managed := ansibleruntime.Commands{Source: ansibleruntime.SourceManaged, Release: "2.21.4",
		Dir: "/srv/st/ansible/2.21.4/bin", Root: "/srv/st/ansible"}
	tests := []struct {
		Inventory         *inventory.Inventory
		Plugin            bool
		Missing           bool
		Commands          ansibleruntime.Commands
		Want              error
		WantEngine        string
		WantAnsibleCore   string
		WantAnsibleSource string
		WantHosts         []string
		WantMessage       []string
	}{{ // Test 0: Static inputs resolve natively though Ansible is installed.
		Inventory: smart, WantEngine: inventory.EngineNative, WantHosts: []string{"web1", "web2"},
	}, { // Test 1: Static inputs resolve natively without Ansible.
		Inventory: smart, Missing: true, WantEngine: inventory.EngineNative,
		WantHosts: []string{"web1", "web2"},
	}, { // Test 2: A plugin configuration input is read by Ansible.
		Inventory: smart, Plugin: true, WantEngine: inventory.EngineAnsible,
		WantAnsibleCore: "2.18.1", WantHosts: []string{"ec2-1", "web1", "web2"},
	}, { // Test 3: A plugin configuration input without Ansible says why, with the install line.
		Inventory: smart, Plugin: true, Missing: true, Want: inventory.ErrNeedsAnsible,
		WantMessage: []string{`input inventory "cloud"`, "plugin configuration",
			inventory.AnsibleInstallHint},
	}, { // Test 4: A constructed inventory is Ansible's.
		Inventory: constructed, WantEngine: inventory.EngineAnsible, WantAnsibleCore: "2.18.1",
		WantHosts: []string{"web1", "web2"},
	}, { // Test 5: A constructed inventory without Ansible says why, with the install line.
		Inventory: constructed, Missing: true, Want: inventory.ErrNeedsAnsible,
		WantMessage: []string{`constructed inventory "built"`, "constructed plugin",
			inventory.AnsibleInstallHint},
	}, { // Test 6: Ansible from the managed runtime is recorded as the managed runtime's.
		Inventory: constructed, Commands: managed, WantEngine: inventory.EngineAnsible,
		WantAnsibleCore: "2.18.1", WantAnsibleSource: ansibleruntime.SourceManaged,
		WantHosts: []string{"web1", "web2"},
	}, { // Test 7: The install line names the runtime directory this server looks in.
		Inventory: constructed, Missing: true, Want: inventory.ErrNeedsAnsible,
		Commands: ansibleruntime.Commands{Source: ansibleruntime.SourcePath,
			Root: "/srv/st/ansible"},
		WantMessage: []string{"Install it with: switchtender ansible install --dir " +
			"/srv/st/ansible, or on the system with: pipx install ansible-core"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			invs := inventory.NewMemStore()
			_ = invs.Save(ctx, &inventory.Inventory{ID: "inv_static", Name: "static",
				Content: "[web]\nweb1\nweb2 tier=gold\n"})
			runner := &inventorytest.ListingRunner{Missing: test.Missing,
				Canned: map[string]string{pluginConfig: pluginListing}, Commands: test.Commands}
			if test.Plugin {
				_ = invs.Save(ctx, &inventory.Inventory{ID: "inv_cloud", Name: "cloud",
					Content: pluginConfig})
			}
			d := New(run.NewMemStore(), runner, zap.NewNop(), WithInventories(invs))
			t.Cleanup(d.Close)
			got, err := d.PreviewInventory(ctx, test.Inventory)
			if !errors.Is(err, test.Want) {
				t.Fatalf("PreviewInventory() error = %v, want %v", err, test.Want)
			}
			for _, part := range test.WantMessage {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not say %q", err, part)
				}
			}
			if err != nil {
				return
			}
			res := got.Resolution
			if res.Engine != test.WantEngine || res.AnsibleCore != test.WantAnsibleCore ||
				res.AnsibleSource != test.WantAnsibleSource {
				t.Errorf("engine = %q %q %q, want %q %q %q", res.Engine, res.AnsibleCore,
					res.AnsibleSource, test.WantEngine, test.WantAnsibleCore, test.WantAnsibleSource)
			}
			if diff := cmp.Diff(test.WantHosts, res.Hosts); diff != "" {
				t.Errorf("hosts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestComposedDigests pins the two digests a resolution records: they are stable for the same
// inputs, the resolved one moves when a resolved variable changes, and neither moves or leaks when
// only a secret value changes, since both are taken over the masked form.
func TestComposedDigests(t *testing.T) {
	t.Parallel()
	resolve := func(content string) *run.InventoryResolution {
		t.Helper()
		ctx := context.Background()
		invs := inventory.NewMemStore()
		_ = invs.Save(ctx, &inventory.Inventory{ID: "inv_a", Name: "a", Content: content})
		d := New(run.NewMemStore(), &inventorytest.ListingRunner{}, zap.NewNop(),
			WithInventories(invs))
		t.Cleanup(d.Close)
		got, err := d.PreviewInventory(ctx, &inventory.Inventory{ID: "inv_s",
			Kind: inventory.KindSmart, HostFilter: "groups__name=web"})
		if err != nil {
			t.Fatalf("PreviewInventory() error = %v", err)
		}
		return got.Resolution
	}
	base := resolve("[web]\nweb1 tier=gold ansible_password=hunter2\n")
	same := resolve("[web]\nweb1 tier=gold ansible_password=hunter2\n")
	moved := resolve("[web]\nweb1 tier=silver ansible_password=hunter2\n")
	secret := resolve("[web]\nweb1 tier=gold ansible_password=correct-horse\n")
	if base.InputDigest != same.InputDigest || base.ResolvedDigest != same.ResolvedDigest {
		t.Errorf("the same inputs gave different digests: %+v, %+v", base, same)
	}
	if base.ResolvedDigest == moved.ResolvedDigest || base.InputDigest == moved.InputDigest {
		t.Errorf("a changed variable did not move the digests: %+v, %+v", base, moved)
	}
	if base.ResolvedDigest != secret.ResolvedDigest || base.InputDigest != secret.InputDigest {
		t.Errorf("a changed secret moved a digest, so the digest commits to the secret: %+v, %+v",
			base, secret)
	}
}

// listingOnly is a runner that can resolve inventories but cannot read them for the cross-check,
// the way an executor without the capability looks to the dispatcher.
type listingOnly struct {
	// lister is the runner it delegates to.
	lister *inventorytest.ListingRunner
}

// Run delegates to the listing runner.
func (l listingOnly) Run(ctx context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	return l.lister.Run(ctx, spec, out)
}

// ListInventory delegates to the listing runner.
func (l listingOnly) ListInventory(ctx context.Context, sources []string, limit string) ([]byte, error) {
	return l.lister.ListInventory(ctx, sources, limit)
}

// TestCrossCheckBeforeAnsibleRuns pins the fail-closed cross-check: an Ansible run against an
// inventory the native engine resolved runs only when Ansible's own reading agrees, records the
// check either way, and is refused with the exact difference otherwise. An executor that cannot
// read the inventory with Ansible refuses the run, and a run that is not Ansible is not checked.
func TestCrossCheckBeforeAnsibleRuns(t *testing.T) {
	t.Parallel()
	asString := func(name string, raw []byte) []byte {
		if name != "input-000" {
			return raw
		}
		return bytes.Replace(raw, []byte(`"port":8080`), []byte(`"port":"8080"`), 1)
	}
	dropHost := func(name string, raw []byte) []byte {
		if name != "inventory" {
			return raw
		}
		return bytes.ReplaceAll(raw, []byte(`"web2"`), []byte(`"web9"`))
	}
	secretDiffers := func(name string, raw []byte) []byte {
		return bytes.ReplaceAll(raw, []byte("hunter2"), []byte("other-secret"))
	}
	tests := []struct {
		Rewrite      func(string, []byte) []byte
		Tool         string
		NoReader     bool
		Missing      bool
		WantStatus   run.Status
		WantChecked  bool
		WantReads    int
		WantInError  []string
		WantNotInErr []string
	}{{ // Test 0: Agreement runs the play and records the check.
		WantStatus: run.StatusSucceeded, WantChecked: true, WantReads: 1,
	}, { // Test 1: An input Ansible types differently refuses the run, naming the variable.
		Rewrite: asString, WantStatus: run.StatusFailed, WantChecked: true, WantReads: 1,
		WantInError: []string{"variable port", "8080 (int)", `"8080" (string)`, "2.18.1"},
	}, { // Test 2: A rendered inventory Ansible reads with other hosts refuses the run.
		Rewrite: dropHost, WantStatus: run.StatusFailed, WantChecked: true, WantReads: 1,
		WantInError: []string{"handed to the play", "host web2", "host web9"},
	}, { // Test 3: A secret that differs is named and never shown.
		Rewrite: secretDiffers, WantStatus: run.StatusFailed, WantChecked: true, WantReads: 1,
		WantInError:  []string{"ansible_password differs"},
		WantNotInErr: []string{"hunter2", "other-secret"},
	}, { // Test 4: An executor that cannot read with Ansible refuses the run.
		NoReader: true, WantStatus: run.StatusFailed, WantInError: []string{"refused"},
	}, { // Test 5: An executor without Ansible refuses the run and says how to install it.
		Missing: true, WantStatus: run.StatusFailed, WantReads: 1,
		WantInError: []string{inventory.AnsibleInstallHint},
	}, { // Test 6: A run that is not Ansible is not checked.
		Tool: "bash", WantStatus: run.StatusSucceeded,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			invs := inventory.NewMemStore()
			_ = invs.Save(ctx, &inventory.Inventory{ID: "inv_a", Name: "fleet",
				Content: "[web]\nweb1 port=8080 ansible_password=hunter2\nweb2\n"})
			_ = invs.Save(ctx, &inventory.Inventory{ID: "inv_s", Name: "webs",
				Kind: inventory.KindSmart, HostFilter: "groups__name=web"})
			lister := &inventorytest.ListingRunner{Rewrite: test.Rewrite, Missing: test.Missing,
				Commands: ansibleruntime.Commands{Source: ansibleruntime.SourceManaged}}
			var runner roundhouse.Runner = lister
			if test.NoReader {
				runner = listingOnly{lister: lister}
			}
			store := run.NewMemStore()
			d := New(store, runner, zap.NewNop(), WithInventories(invs))
			t.Cleanup(d.Close)
			opts := []run.SubmitOption{run.WithInventory("inv_s")}
			playbook := "site.yml"
			if test.Tool != "" {
				playbook = ""
				opts = append(opts, run.WithTool(test.Tool), run.WithCommand("true"))
			}
			created, err := d.Submit(ctx, playbook, "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			done := waitTerminal(t, store, created.ID)
			if done.Status != test.WantStatus {
				t.Fatalf("status = %s (%s), want %s", done.Status, done.Error, test.WantStatus)
			}
			if got := done.InventoryCheck != nil; got != test.WantChecked {
				t.Errorf("check recorded = %v, want %v: %+v", got, test.WantChecked, done.InventoryCheck)
			}
			if c := done.InventoryCheck; c != nil && test.WantStatus == run.StatusSucceeded {
				res := done.InventoryResolution
				if c.AnsibleCore != "2.18.1" || c.AnsibleSource != ansibleruntime.SourceManaged ||
					c.InputDigest != res.InputDigest || c.ResolvedDigest != res.ResolvedDigest {
					t.Errorf("check %+v does not match the resolution %+v", c, res)
				}
			}
			if got := lister.Reads(); got != test.WantReads {
				t.Errorf("cross-check reads = %d, want %d", got, test.WantReads)
			}
			for _, part := range test.WantInError {
				if !strings.Contains(done.Error, part) {
					t.Errorf("error %q does not say %q", done.Error, part)
				}
			}
			for _, part := range test.WantNotInErr {
				check := fmt.Sprint(done.InventoryCheck)
				if strings.Contains(done.Error, part) || strings.Contains(check, part) {
					t.Errorf("the refusal leaks %q: %s", part, done.Error)
				}
			}
			if test.WantStatus == run.StatusFailed && len(lister.Executed()) > 0 {
				t.Error("the play ran although the cross-check refused it")
			}
		})
	}
}
