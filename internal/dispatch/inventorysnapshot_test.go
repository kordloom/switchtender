package dispatch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// snapshotSealer is the key the snapshot tests seal with.
var snapshotSealer = credential.NewSealer("snapshot-dispatch-pass", "snapshot-dispatch-salt")

// errNoClaim stops a dispatcher from claiming, so a test can change the world between a submission
// and the claim that executes it.
var errNoClaim = errors.New("this dispatcher only submits")

// snapshotRunner records the inventory content and environment each execution is handed, and, as an
// InventoryReader, answers for a dynamic source with a listing a test chose.
type snapshotRunner struct {
	// mu guards seen and env.
	mu sync.Mutex
	// seen holds the inventory content each execution was handed, in order.
	seen []string
	// env holds the environment each execution was handed, in order.
	env [][]string
	// listing is what ReadInventories reports for a dynamic source.
	listing string
}

// Run records the inventory content and environment it was handed, and succeeds.
func (s *snapshotRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result,
	error) {
	content := ""
	if spec.Inventory != "" {
		b, err := os.ReadFile(spec.Inventory)
		if err != nil {
			return roundhouse.Result{ExitCode: 1}, err
		}
		content = string(b)
	}
	s.mu.Lock()
	s.seen = append(s.seen, content)
	s.env = append(s.env, append([]string(nil), spec.Env...))
	s.mu.Unlock()
	return roundhouse.Result{ExitCode: 0}, nil
}

// ReadInventories answers every file with the listing the test chose, the way ansible-inventory
// answers for a dynamic source by asking the live system it describes.
func (s *snapshotRunner) ReadInventories(_ context.Context, _ roundhouse.Spec, _ string,
	names []string) ([][]byte, string, error) {
	out := make([][]byte, len(names))
	for i := range names {
		out[i] = []byte(s.listing)
	}
	return out, "2.18.1", nil
}

// executions returns the inventory content each execution was handed.
func (s *snapshotRunner) executions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// environments returns the environment each execution was handed.
func (s *snapshotRunner) environments() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.env...)
}

// snapshotFixture is a store, an inventory store, and a runner the snapshot tests share.
type snapshotFixture struct {
	// Store is the run store.
	Store run.Store
	// Inventories holds the stored inventories.
	Inventories inventory.Store
	// Credentials holds the stored credentials.
	Credentials credential.Store
	// Runner records what each execution was handed.
	Runner *snapshotRunner
}

// newSnapshotFixture builds an empty fixture.
func newSnapshotFixture() *snapshotFixture {
	return &snapshotFixture{Store: run.NewMemStore(), Inventories: inventory.NewMemStore(),
		Credentials: credential.NewMemStore(), Runner: &snapshotRunner{}}
}

// dispatcher returns a dispatcher over the fixture with the snapshot sealer, adding opts.
func (f *snapshotFixture) dispatcher(t *testing.T, opts ...Option) *Dispatcher {
	t.Helper()
	base := []Option{WithInventories(f.Inventories), WithCredentials(f.Credentials, snapshotSealer),
		WithRunFilesRoot(t.TempDir()), WithNoJanitor()}
	d := New(f.Store, f.Runner, nil, append(base, opts...)...)
	t.Cleanup(d.Close)
	return d
}

// saveInventory stores an inventory with the given content and attached credentials.
func (f *snapshotFixture) saveInventory(t *testing.T, content string, creds ...string) {
	t.Helper()
	if err := f.Inventories.Save(context.Background(), &inventory.Inventory{
		ID: "inv_snap", Name: "web", Content: content, CredentialIDs: creds,
	}); err != nil {
		t.Fatalf("Save(inventory) error = %v", err)
	}
}

// TestAnUnheldRunExecutesTheInventoryItWasSubmittedWith pins the snapshot for a run nothing held. The
// gate graded and admitted the run against the inventory as it stood at submission, so an edit that
// lands before the run is claimed, by a queue backed up behind a long run or a worker that is busy,
// must not change the hosts it reaches. Only the claiming dispatcher executes, after the edit.
func TestAnUnheldRunExecutesTheInventoryItWasSubmittedWith(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSnapshotFixture()
	f.saveInventory(t, "[web]\ncanary\n")
	submitter := f.dispatcher(t, WithClaimGate(func() error { return errNoClaim }))
	submitted, err := submitter.Submit(ctx, "site.yml", "", run.WithTool(run.ToolAnsible),
		run.WithInventory("inv_snap"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if submitted.Status != run.StatusPending {
		t.Fatalf("status = %q, want pending", submitted.Status)
	}
	f.saveInventory(t, "[web]\nprod1\nprod2\n")
	f.dispatcher(t)
	final := waitTerminal(t, f.Store, submitted.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", final.Status, final.Error)
	}
	if diff := cmp.Diff([]string{"[web]\ncanary\n"}, f.Runner.executions()); diff != "" {
		t.Errorf("executed inventory mismatch (-want +got):\n%s", diff)
	}
	if final.InventorySnapshot == nil ||
		!cmp.Equal([]string{"canary"}, final.InventorySnapshot.Hosts) {
		t.Errorf("snapshot record = %+v, want the canary host bound", final.InventorySnapshot)
	}
}

// TestARunWhoseSnapshotCannotBeTrustedIsRefused covers the ways the snapshot can be missing or
// wrong. Each refuses the run before any tool starts, rather than executing whatever the inventory
// holds when it is claimed, which is the failure the snapshot exists to remove.
func TestARunWhoseSnapshotCannotBeTrustedIsRefused(t *testing.T) {
	t.Parallel()
	const content = "[web]\ncanary\n"
	sealed, err := snapshotSealer.Seal(base64.StdEncoding.EncodeToString([]byte(content)))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	other := credential.NewSealer("another-pass", "another-salt")
	foreign, err := other.Seal(base64.StdEncoding.EncodeToString([]byte(content)))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	swapped, err := snapshotSealer.Seal(base64.StdEncoding.EncodeToString([]byte("[web]\nprod1\n")))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	plain := plainPrefix + base64.StdEncoding.EncodeToString([]byte(content))
	record := func(blob string) *run.InventorySnapshot {
		return &run.InventorySnapshot{SealedSHA256: run.SealedBlobSHA256(blob),
			ContentSHA256: maskedContentSHA256(content), Hosts: []string{"canary"}}
	}
	tests := []struct {
		// Name labels the case.
		Name string
		// Snapshot is the record the run binds.
		Snapshot *run.InventorySnapshot
		// Sealed is the sealed content stored on the run.
		Sealed string
		// WantError is what the refusal must say.
		WantError string
	}{{ // Test 0: A run naming a stored inventory with no snapshot at all.
		Name: "no snapshot", WantError: "carries no snapshot",
	}, { // Test 1: The record survives and the sealed content is gone.
		Name: "missing content", Snapshot: record(sealed), WantError: "is missing",
	}, { // Test 2: Other sealed content stored under the record that bound the first.
		Name: "swapped content", Snapshot: record(sealed), Sealed: swapped, WantError: "changed after",
	}, { // Test 3: Content sealed under another key does not open here.
		Name: "foreign key", Snapshot: record(foreign), Sealed: foreign, WantError: "does not open",
	}, { // Test 4: Content stored plain on an install that holds a key.
		Name: "stored plain", Snapshot: record(plain), Sealed: plain, WantError: "does not open",
	}, { // Test 5: Content that opens but is not what the masked digest bound.
		Name:      "content differs from the bound digest",
		Snapshot:  &run.InventorySnapshot{SealedSHA256: run.SealedBlobSHA256(swapped), ContentSHA256: maskedContentSHA256(content)},
		Sealed:    swapped,
		WantError: "not the content",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newSnapshotFixture()
			f.saveInventory(t, content)
			stored := &run.Run{
				ID: "run_untrusted", Playbook: "site.yml", Tool: run.ToolAnsible, InventoryID: "inv_snap",
				Status: run.StatusPending, CreatedAt: time.Now(),
				InventorySnapshot: test.Snapshot, InventorySealed: test.Sealed,
			}
			if err := f.Store.Save(ctx, stored); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			f.dispatcher(t)
			final := waitTerminal(t, f.Store, stored.ID)
			if final.Status != run.StatusFailed || !strings.Contains(final.Error, test.WantError) {
				t.Errorf("run ended %q (%q), want failed saying %q", final.Status, final.Error,
					test.WantError)
			}
			if got := f.Runner.executions(); len(got) != 0 {
				t.Errorf("a tool ran against %q although the snapshot could not be trusted", got)
			}
		})
	}
}

// TestTheSnapshotIsSealedAtRestAndWipedWhenTheRunEnds pins how the snapshot is kept. Inventory content
// can carry an ansible_password, so the stored snapshot is ciphertext, never the content, and it is
// wiped by the run's end while the record that bound it stays for the evidence. A run ends through
// its execution or through a decision that it never executes, and both ends wipe it.
func TestTheSnapshotIsSealedAtRestAndWipedWhenTheRunEnds(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-inventory-secret"
	tests := []struct {
		// Name labels the case.
		Name string
		// Reject decides the held run against executing it.
		Reject bool
		// WantStatus is how the run ends.
		WantStatus run.Status
	}{{ // Test 0: The approved run executes and ends.
		Name: "executed", WantStatus: run.StatusSucceeded,
	}, { // Test 1: The run is rejected and never executes.
		Name: "rejected", Reject: true, WantStatus: run.StatusRejected,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newSnapshotFixture()
			f.saveInventory(t, "[web]\ncanary ansible_password="+secret+"\n")
			d := f.dispatcher(t, WithAudits(audit.NewMemStore()))
			held, err := d.Submit(ctx, "site.yml", "", run.WithTool(run.ToolAnsible),
				run.WithInventory("inv_snap"), run.WithRequireApproval(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			waiting, err := f.Store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if waiting.InventorySealed == "" {
				t.Fatal("the waiting run carries no sealed snapshot")
			}
			encoded := base64.StdEncoding.EncodeToString(
				[]byte("[web]\ncanary ansible_password=" + secret + "\n"))
			if strings.Contains(waiting.InventorySealed, secret) ||
				strings.Contains(waiting.InventorySealed, encoded) ||
				strings.HasPrefix(waiting.InventorySealed, plainPrefix) {
				t.Errorf("the stored snapshot is not sealed: %q", waiting.InventorySealed)
			}
			if test.Reject {
				_, err = d.Reject(ctx, held.ID, "not now", decider("approver-1", "session"))
			} else {
				_, err = d.Approve(ctx, held.ID, decider("approver-1", "session"))
			}
			if err != nil {
				t.Fatalf("decision error = %v", err)
			}
			final := waitTerminal(t, f.Store, held.ID)
			if final.Status != test.WantStatus {
				t.Fatalf("status = %q (%s), want %q", final.Status, final.Error, test.WantStatus)
			}
			if final.InventorySealed != "" {
				t.Errorf("the ended run still carries its sealed snapshot")
			}
			if final.InventorySnapshot == nil || final.InventorySnapshot.SealedSHA256 == "" {
				t.Errorf("the wipe removed the record that bound the snapshot: %+v",
					final.InventorySnapshot)
			}
		})
	}
}

// TestADynamicSourceRecordsTheHostsItResolvedTo pins what a run against a dynamic inventory source
// records. Its hosts exist only when Ansible asks the live system, so they cannot be bound at
// approval: they are resolved once at execution, recorded on the run for the outcome to commit, and
// the play is handed that resolution, so the recorded hosts are the hosts the play reached.
func TestADynamicSourceRecordsTheHostsItResolvedTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSnapshotFixture()
	f.Runner.listing = `{"_meta":{"hostvars":{"ec2-b":{"zone":"b"}}},` +
		`"all":{"children":["aws"]},"aws":{"hosts":["ec2-a","ec2-b"]}}`
	f.saveInventory(t, "plugin: amazon.aws.aws_ec2\nregions:\n  - us-east-1\n")
	d := f.dispatcher(t)
	submitted, err := d.Submit(ctx, "site.yml", "", run.WithTool(run.ToolAnsible),
		run.WithInventory("inv_snap"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if submitted.InventorySnapshot == nil || !submitted.InventorySnapshot.Dynamic ||
		len(submitted.InventorySnapshot.Hosts) != 0 {
		t.Fatalf("snapshot record = %+v, want a dynamic source with no hosts bound",
			submitted.InventorySnapshot)
	}
	final := waitTerminal(t, f.Store, submitted.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", final.Status, final.Error)
	}
	if diff := cmp.Diff([]string{"ec2-a", "ec2-b"}, final.ResolvedHosts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("resolved hosts mismatch (-want +got):\n%s", diff)
	}
	seen := f.Runner.executions()
	if len(seen) != 1 || !strings.Contains(seen[0], "ec2-a") || strings.Contains(seen[0], "plugin:") {
		t.Errorf("the play was handed %q, want the resolution it recorded", seen)
	}
}

// TestACredentialAttachedToTheInventoryAfterSubmissionIsNotApplied pins the inventory's attached
// credentials to the snapshot. Every run against an inventory receives what it attaches, so a
// credential attached after a run was submitted, a become password or a type that injects variables,
// would change what an approved run executes with.
func TestACredentialAttachedToTheInventoryAfterSubmissionIsNotApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSnapshotFixture()
	const value = "LATE_ATTACHED_VALUE"
	sealedEnv, err := snapshotSealer.Seal("LATE_VAR=" + value)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if err := f.Credentials.Save(ctx, &credential.Credential{
		ID: "cred_late", Name: "late", Kind: credential.KindEnv, Secret: sealedEnv,
	}); err != nil {
		t.Fatalf("Save(credential) error = %v", err)
	}
	f.saveInventory(t, "[web]\ncanary\n")
	submitter := f.dispatcher(t, WithClaimGate(func() error { return errNoClaim }))
	submitted, err := submitter.Submit(ctx, "site.yml", "", run.WithTool(run.ToolAnsible),
		run.WithInventory("inv_snap"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	f.saveInventory(t, "[web]\ncanary\n", "cred_late")
	f.dispatcher(t)
	final := waitTerminal(t, f.Store, submitted.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", final.Status, final.Error)
	}
	for _, env := range f.Runner.environments() {
		for _, kv := range env {
			if strings.Contains(kv, value) {
				t.Errorf("the run executed with a credential attached after it was submitted: %q", kv)
			}
		}
	}
}

// TestOpenSecretsDeliversOnlyTheBoundSnapshot pins what a relay worker is handed. The control node
// opens the snapshot for the worker's pool only after holding its sealed form to the run's digest,
// and a worker holds what arrives to the masked digest the run binds, since the sealed form never
// leaves the control node.
func TestOpenSecretsDeliversOnlyTheBoundSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSnapshotFixture()
	f.saveInventory(t, "[web]\ncanary\n")
	d := f.dispatcher(t, WithClaimGate(func() error { return errNoClaim }))
	submitted, err := d.Submit(ctx, "site.yml", "", run.WithTool(run.ToolAnsible),
		run.WithInventory("inv_snap"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	stored, err := f.Store.Get(ctx, submitted.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !d.NeedsSecrets(ctx, stored) {
		t.Error("a run carrying an inventory snapshot reports nothing to deliver")
	}
	payload, release, err := d.OpenSecrets(ctx, stored)
	if err != nil {
		t.Fatalf("OpenSecrets() error = %v", err)
	}
	if release != nil {
		release()
	}
	if payload.Inventory != "[web]\ncanary\n" {
		t.Errorf("delivered inventory = %q, want the snapshot", payload.Inventory)
	}
	// A worker refuses delivered content the run's record does not bind.
	deliveries := newHeldDeliveries()
	deliveries.payloads[stored.ID] = &handoff.Payload{RunID: stored.ID, Inventory: "[web]\nprod1\n"}
	src := &deliveredSource{r: stored, recv: deliveries}
	if _, err := src.inventorySnapshot(ctx, stored); !errors.Is(err, ErrInventorySnapshot) {
		t.Errorf("a worker accepted delivered content the run does not bind: %v", err)
	}
	// And the control node refuses to open a snapshot whose sealed form changed.
	tampered := stored.Clone()
	tampered.InventorySealed = stored.InventorySealed + "A"
	if _, _, err := d.OpenSecrets(ctx, tampered); !errors.Is(err, ErrInventorySnapshot) {
		t.Errorf("OpenSecrets() opened a changed snapshot: %v", err)
	}
}
