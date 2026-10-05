package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// composedSnapshotFixture is a store of runs, the compose fixture's inputs plus a smart inventory
// over the web hosts that attaches a credential, a listing runner, and a dispatcher that seals with
// the snapshot key and only submits until gate is closed.
type composedSnapshotFixture struct {
	// Store is the run store.
	Store run.Store
	// Inventories holds the inputs and the smart inventory.
	Inventories inventory.Store
	// Runner records the inventory each execution was handed.
	Runner *inventorytest.ListingRunner
	// D is the dispatcher.
	D *Dispatcher
	// open lets the dispatcher claim once it is closed.
	open chan struct{}
}

// newComposedSnapshotFixture builds the fixture.
func newComposedSnapshotFixture(t *testing.T) *composedSnapshotFixture {
	t.Helper()
	f := &composedSnapshotFixture{Store: run.NewMemStore(), Inventories: composeInventories(t),
		Runner: &inventorytest.ListingRunner{}, open: make(chan struct{})}
	creds := credential.NewMemStore()
	sealedEnv, err := snapshotSealer.Seal("WEB_TOKEN=composed-web-token")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "cred_web", Name: "web", Kind: credential.KindEnv, Secret: sealedEnv,
	}); err != nil {
		t.Fatalf("Save(credential) error = %v", err)
	}
	if err := f.Inventories.Save(context.Background(), &inventory.Inventory{
		ID: "inv_smart", Name: "web everywhere", Kind: inventory.KindSmart, OrgID: "org_x",
		HostFilter: "groups__name=web", CredentialIDs: []string{"cred_web"},
	}); err != nil {
		t.Fatalf("Save(smart) error = %v", err)
	}
	f.D = New(f.Store, f.Runner, zap.NewNop(), WithInventories(f.Inventories),
		WithCredentials(creds, snapshotSealer), WithRunFilesRoot(t.TempDir()),
		WithNoJanitor(), WithClaimGate(func() error {
			select {
			case <-f.open:
				return nil
			default:
				return errNoClaim
			}
		}))
	t.Cleanup(f.D.Close)
	return f
}

// release lets the dispatcher claim and execute.
func (f *composedSnapshotFixture) release() {
	close(f.open)
	f.D.wake()
}

// TestAComposedRunExecutesItsSnapshot pins that a composed inventory is snapshotted when the run is
// submitted, through the record a plain inventory takes, and executed from that snapshot. Every
// input is read once, at submission, so an input edited afterward, a host variable or a secret
// included, does not reach the run, and the approval binds the composed result: its hosts, a masked
// digest of its content, the digest of the sealed snapshot, and the credentials the composed
// inventory attaches.
func TestAComposedRunExecutesItsSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newComposedSnapshotFixture(t)
	created, err := f.D.Submit(ctx, "site.yml", "", run.WithInventory("inv_smart"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	snap := created.InventorySnapshot
	if snap == nil || created.InventorySealed == "" {
		t.Fatal("the composed run carries no snapshot")
	}
	if diff := cmp.Diff([]string{"web1", "web2"}, snap.Hosts); diff != "" {
		t.Errorf("snapshot hosts mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"cred_web"}, snap.CredentialIDs); diff != "" {
		t.Errorf("snapshot credentials mismatch (-want +got):\n%s", diff)
	}
	if snap.SealedSHA256 != run.SealedBlobSHA256(created.InventorySealed) ||
		strings.HasPrefix(created.InventorySealed, plainPrefix) {
		t.Error("the snapshot is not sealed under the server's key and bound by its digest")
	}
	spec, err := outcome.Spec(created)
	if err != nil {
		t.Fatalf("outcome.Spec() error = %v", err)
	}
	if !strings.Contains(string(spec), snap.SealedSHA256) ||
		!strings.Contains(string(spec), snap.ContentSHA256) {
		t.Errorf("the committed spec does not bind the composed snapshot: %s", spec)
	}

	// An input changes after the submission: web1 is pointed somewhere else and given a password.
	if err := f.Inventories.Save(ctx, &inventory.Inventory{
		ID: "inv_a", Name: "production", OrgID: "org_x",
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Content: inventorytest.Listing(map[string][]string{"web": {"web1", "web2"}},
			map[string]map[string]any{"web1": {"env": "redirected", "ansible_password": "hunter2"},
				"web2": {"env": "dev"}}),
	}); err != nil {
		t.Fatalf("Save(inv_a) error = %v", err)
	}
	f.release()
	done := waitTerminal(t, f.Store, created.ID)
	if done.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", done.Status, done.Error)
	}
	executed := f.Runner.Executed()
	if len(executed) != 1 {
		t.Fatalf("runner ran %d times, want 1", len(executed))
	}
	if !strings.Contains(executed[0], `"prod"`) {
		t.Errorf("the executed inventory lost the variable web1 had at submission:\n%s", executed[0])
	}
	for _, edit := range []string{"redirected", "hunter2"} {
		if strings.Contains(executed[0], edit) {
			t.Errorf("the executed inventory took %q from an input edited after submission:\n%s", edit,
				executed[0])
		}
	}
	if c := done.InventoryCheck; c == nil || c.InputDigest != created.InventoryResolution.InputDigest {
		t.Errorf("the cross-check %+v did not read the inputs the run resolved from", done.InventoryCheck)
	}
}

// TestAComposedRunWhoseSnapshotCannotBeTrustedIsRefused covers the ways a composed run's snapshot
// can be missing or wrong. Each refuses the run before any tool starts, rather than composing its
// inputs again as they stand when it is claimed.
func TestAComposedRunWhoseSnapshotCannotBeTrustedIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Spoil changes the stored run before it is claimed.
		Spoil func(t *testing.T, r *run.Run)
		// WantError is what the refusal must say.
		WantError string
	}{{ // Test 0: A composed run with a resolution and no snapshot.
		Name: "no snapshot",
		Spoil: func(_ *testing.T, r *run.Run) {
			r.InventorySnapshot, r.InventorySealed = nil, ""
		},
		WantError: "carries no snapshot",
	}, { // Test 1: The record survives and the sealed content is gone.
		Name:      "missing content",
		Spoil:     func(_ *testing.T, r *run.Run) { r.InventorySealed = "" },
		WantError: "is missing",
	}, { // Test 2: Other sealed content stored under the record that bound the first.
		Name: "swapped content",
		Spoil: func(t *testing.T, r *run.Run) {
			r.InventorySealed = sealComposed(t, composedSnapshot{Content: `{"all":{}}`})
		},
		WantError: "changed after",
	}, { // Test 3: Inputs that are not the ones the resolution bound, under a matching record.
		Name: "inputs differ",
		Spoil: func(t *testing.T, r *run.Run) {
			cs := openComposed(t, r)
			cs.Inputs[0].Content = strings.Replace(cs.Inputs[0].Content, `"prod"`, `"edited"`, 1)
			rebind(t, r, cs)
		},
		WantError: "not the ones this run resolved from",
	}, { // Test 4: A composed result that is not the content the masked digest bound.
		Name: "result differs",
		Spoil: func(t *testing.T, r *run.Run) {
			cs := openComposed(t, r)
			cs.Content = strings.Replace(cs.Content, `"web2"`, `"web9"`, 1)
			rebind(t, r, cs)
		},
		WantError: "not the content",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newComposedSnapshotFixture(t)
			created, err := f.D.Submit(ctx, "site.yml", "", run.WithInventory("inv_smart"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			// The spoiled run is stored as a fresh row, since a save never rewrites sealed material.
			spoiled := created.Clone()
			spoiled.ID = "run_spoiled"
			test.Spoil(t, spoiled)
			if err := f.Store.Save(ctx, spoiled); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if canceled, err := f.D.CancelWaiting(ctx, created.ID); err != nil || !canceled {
				t.Fatalf("CancelWaiting() = %v, %v, want the unspoiled run canceled", canceled, err)
			}
			f.release()
			final := waitTerminal(t, f.Store, spoiled.ID)
			if final.Status != run.StatusFailed || !strings.Contains(final.Error, test.WantError) {
				t.Errorf("run ended %q (%q), want failed saying %q", final.Status, final.Error,
					test.WantError)
			}
			if !strings.Contains(final.Error, ErrInventorySnapshot.Error()) {
				t.Errorf("the refusal %q does not name the snapshot", final.Error)
			}
			if got := f.Runner.Executed(); len(got) != 0 {
				t.Errorf("a tool ran against %q although the snapshot could not be trusted", got)
			}
		})
	}
}

// sealComposed seals a composed snapshot under the snapshot key.
func sealComposed(t *testing.T, cs composedSnapshot) string {
	t.Helper()
	raw, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	d := &Dispatcher{sealer: snapshotSealer}
	sealed, err := d.sealBytes(raw)
	if err != nil {
		t.Fatalf("sealBytes() error = %v", err)
	}
	return sealed
}

// openComposed opens r's composed snapshot under the snapshot key.
func openComposed(t *testing.T, r *run.Run) *composedSnapshot {
	t.Helper()
	d := &Dispatcher{sealer: snapshotSealer}
	raw, err := d.openBytes(r.InventorySealed)
	if err != nil {
		t.Fatalf("openBytes() error = %v", err)
	}
	cs, err := parseComposedSnapshot(string(raw))
	if err != nil {
		t.Fatalf("parseComposedSnapshot() error = %v", err)
	}
	return cs
}

// rebind seals cs onto r and binds its sealed digest, so the content check is the one that refuses.
func rebind(t *testing.T, r *run.Run, cs *composedSnapshot) {
	t.Helper()
	r.InventorySealed = sealComposed(t, *cs)
	r.InventorySnapshot.SealedSHA256 = run.SealedBlobSHA256(r.InventorySealed)
}

// TestOpenSecretsDeliversTheComposedSnapshot pins what a relay worker receives for a composed run:
// the sealed snapshot opened for its pool, the result and the inputs together, which the worker
// holds to the digests the run binds before it executes anything, so a worker reads no input from
// the inventory store and a tampered delivery is refused.
func TestOpenSecretsDeliversTheComposedSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newComposedSnapshotFixture(t)
	created, err := f.D.Submit(ctx, "site.yml", "", run.WithInventory("inv_smart"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	payload, release, err := f.D.OpenSecrets(ctx, created)
	if err != nil {
		t.Fatalf("OpenSecrets() error = %v", err)
	}
	if release != nil {
		release()
	}
	if err := checkSnapshotContent(created, payload.Inventory); err != nil {
		t.Fatalf("the delivered snapshot does not hold to the run's digests: %v", err)
	}
	comp, err := composedFromSnapshot(created, payload.Inventory)
	if err != nil {
		t.Fatalf("composedFromSnapshot() error = %v", err)
	}
	if len(comp.inputs) != 1 || comp.inputs[0].inv.ID != "inv_a" ||
		comp.inputs[0].engine != inventory.EngineNative {
		t.Errorf("delivered inputs = %+v, want inv_a read natively", comp.inputs)
	}
	cs, err := parseComposedSnapshot(payload.Inventory)
	if err != nil {
		t.Fatalf("parseComposedSnapshot() error = %v", err)
	}
	cs.Inputs[0].Content = strings.Replace(cs.Inputs[0].Content, `"prod"`, `"edited"`, 1)
	raw, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := checkSnapshotContent(created, string(raw)); !errors.Is(err, ErrInventorySnapshot) {
		t.Errorf("a delivery with an altered input was accepted: %v", err)
	}
}

// TestARetryHeldToAResolutionTakesItsOwnSnapshot pins the one composed run that arrives with a
// resolution and no snapshot: the retry of a split's failed shards, whose parent's snapshot was
// wiped when the parent ended. The retry is a submission of its own, so it is composed again from
// its inputs as they stand, held to the hosts the resolution recorded, and snapshotted, so it
// executes what its own submission recorded rather than being refused or reading its inputs when it
// runs.
func TestARetryHeldToAResolutionTakesItsOwnSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newComposedSnapshotFixture(t)
	launched, err := f.D.Submit(ctx, "site.yml", "", run.WithInventory("inv_smart"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	// An input gains a host and changes a variable before the retry is submitted.
	if err := f.Inventories.Save(ctx, &inventory.Inventory{
		ID: "inv_a", Name: "production", OrgID: "org_x",
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Content: inventorytest.Listing(map[string][]string{"web": {"web1", "web2", "web3"}},
			map[string]map[string]any{"web1": {"env": "retried"}, "web2": {"env": "dev"}}),
	}); err != nil {
		t.Fatalf("Save(inv_a) error = %v", err)
	}
	retry := &run.Run{ID: "run_retry", Playbook: "site.yml", InventoryID: "inv_smart",
		InventoryResolution: launched.InventoryResolution.Clone()}
	if err := f.D.snapshotInventory(ctx, retry); err != nil {
		t.Fatalf("snapshotInventory() error = %v", err)
	}
	if retry.InventorySnapshot == nil || retry.InventorySealed == "" {
		t.Fatal("the retry carries no snapshot, so it would be refused when it runs")
	}
	if diff := cmp.Diff([]string{"web1", "web2"}, retry.InventorySnapshot.Hosts); diff != "" {
		t.Errorf("the retry's hosts mismatch, want the resolution's (-want +got):\n%s", diff)
	}
	raw, err := f.D.openBytes(retry.InventorySealed)
	if err != nil {
		t.Fatalf("openBytes() error = %v", err)
	}
	if err := checkSnapshotContent(retry, string(raw)); err != nil {
		t.Errorf("the retry's snapshot does not hold to its own record: %v", err)
	}
	cs, err := parseComposedSnapshot(string(raw))
	if err != nil {
		t.Fatalf("parseComposedSnapshot() error = %v", err)
	}
	if !strings.Contains(cs.Content, "retried") || strings.Contains(cs.Content, "web3") {
		t.Errorf("the retry's snapshot = %s, want the inputs as they stand, held to the resolution",
			cs.Content)
	}
}
