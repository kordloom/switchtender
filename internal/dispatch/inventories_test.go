package dispatch

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/railwarden/internal/credential"
	"github.com/dcadolph/railwarden/internal/inventory"
	"github.com/dcadolph/railwarden/internal/roundhouse"
	"github.com/dcadolph/railwarden/internal/run"
)

// TestInventoryContent covers resolving an inventory's content from its source: a local source
// returns the stored content, a command source runs the sealed command and returns its stdout, and
// a non-local source fails when the config cannot be decrypted or no sealer is configured.
func TestInventoryContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Source      string
		Content     string
		Command     string
		BadConfig   bool
		NilSealer   bool
		WantContent string
		WantErr     bool
	}{{ // Test 0: An empty source returns the stored content.
		Name: "local default", Content: "[web]\nhost1", WantContent: "[web]\nhost1",
	}, { // Test 1: An explicit local source returns the stored content.
		Name: "local explicit", Source: credential.SourceLocal, Content: "[db]\nhost2",
		WantContent: "[db]\nhost2",
	}, { // Test 2: A command source resolves the content from the command's stdout.
		Name: "command", Source: credential.SourceCommand, Command: "printf '[web]\\nhost1\\n'",
		WantContent: "[web]\nhost1",
	}, { // Test 3: A non-local source with no sealer is an error.
		Name: "no sealer", Source: credential.SourceCommand, Command: "printf x",
		NilSealer: true, WantErr: true,
	}, { // Test 4: A non-local source whose config will not decrypt is an error.
		Name: "bad config", Source: credential.SourceCommand, BadConfig: true, WantErr: true,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			sealer := credential.NewSealer("pass", "salt")
			inv := &inventory.Inventory{ID: "inv_1", ContentSource: test.Source, Content: test.Content}
			if test.Command != "" {
				sealed, err := sealer.Seal(test.Command)
				if err != nil {
					t.Fatalf("Seal() error = %v", err)
				}
				inv.ContentConfig = sealed
			}
			if test.BadConfig {
				inv.ContentConfig = "not-a-sealed-value"
			}
			d := &Dispatcher{sealer: sealer}
			if test.NilSealer {
				d.sealer = nil
			}

			got, err := d.inventoryContent(context.Background(), inv)
			if test.WantErr {
				if err == nil {
					t.Fatal("inventoryContent() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("inventoryContent() error = %v", err)
			}
			if diff := cmp.Diff(test.WantContent, got); diff != "" {
				t.Errorf("content mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestInventoryFileCommandSource drives the full materialization path: a stored inventory whose
// content comes from a sealed command is fetched, resolved, and written to a temp file the executor
// reads, proving the resolved host list reaches the run.
func TestInventoryFileCommandSource(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("printf '[web]\\nhost1\\n'")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := inventory.NewMemStore()
	if err := store.Save(context.Background(), &inventory.Inventory{
		ID: "inv_1", Name: "prod", ContentSource: credential.SourceCommand, ContentConfig: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d := &Dispatcher{inventories: store, sealer: sealer}
	path, cleanup, _, err := d.inventoryFile(context.Background(), "inv_1")
	defer cleanup()
	if err != nil {
		t.Fatalf("inventoryFile() error = %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if diff := cmp.Diff("[web]\nhost1", string(body)); diff != "" {
		t.Errorf("materialized inventory mismatch (-want +got):\n%s", diff)
	}
}

// TestRunResolvesInventoryContentSource drives a full run through the dispatcher: a submitted run
// targets a stored inventory whose content comes from a sealed command, and the runner receives the
// resolved host list at spec.Inventory, proving the sourced content reaches execution.
func TestRunResolvesInventoryContentSource(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("printf '[web]\\nhostX\\n'")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	invStore := inventory.NewMemStore()
	if err := invStore.Save(context.Background(), &inventory.Inventory{
		ID: "inv_src", Name: "sourced", ContentSource: credential.SourceCommand, ContentConfig: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var mu sync.Mutex
	var gotInventory string
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			body, readErr := os.ReadFile(spec.Inventory)
			mu.Lock()
			gotInventory = string(body)
			mu.Unlock()
			if readErr != nil {
				return roundhouse.Result{}, readErr
			}
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)

	store := run.NewMemStore()
	d := New(store, runner, nil, WithInventories(invStore),
		WithCredentials(credential.NewMemStore(), sealer))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "", run.WithInventory("inv_src"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitTerminal(t, store, created.ID)
	if got.Status != run.StatusSucceeded {
		t.Fatalf("run status = %q, want succeeded", got.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if diff := cmp.Diff("[web]\nhostX", gotInventory); diff != "" {
		t.Errorf("inventory the runner received (-want +got):\n%s", diff)
	}
}

func TestInventorySecrets(t *testing.T) {
	t.Parallel()
	content := "[db]\ndb01 ansible_password=hunter2 ansible_user=deploy\n" +
		"[web:vars]\napi_token: tok-abc123\nplain_var=notsecret\n"
	got := inventorySecrets(content)
	want := map[string]bool{"hunter2": true, "tok-abc123": true}
	for _, v := range got {
		if !want[v] {
			t.Errorf("inventorySecrets returned unexpected value %q", v)
			continue
		}
		delete(want, v)
	}
	if len(want) != 0 {
		t.Errorf("inventorySecrets missing %v, got %v", want, got)
	}
}

// TestInventoryQueuePinning covers the queue fallback at submit: a run inherits its stored
// inventory's queue unless the request already pinned one, and a run with no stored inventory or
// an unpinned inventory keeps the default pool.
func TestInventoryQueuePinning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name           string
		RunQueue       string
		InventoryID    string
		InventoryQueue string
		WantQueue      string
	}{{ // Test 0: The inventory queue pins the run when the request has none.
		Name: "inherit", InventoryID: "inv_1", InventoryQueue: "dmz", WantQueue: "dmz",
	}, { // Test 1: An explicit run queue outranks the inventory queue.
		Name: "run wins", RunQueue: "gpu", InventoryID: "inv_1", InventoryQueue: "dmz", WantQueue: "gpu",
	}, { // Test 2: An unpinned inventory leaves the run on the default pool.
		Name: "unpinned inventory", InventoryID: "inv_1", WantQueue: "",
	}, { // Test 3: No stored inventory leaves the run on the default pool.
		Name: "no inventory", WantQueue: "",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			invStore := inventory.NewMemStore()
			if err := invStore.Save(context.Background(), &inventory.Inventory{
				ID: "inv_1", Name: "fleet", Content: "[web]\nhost1", Queue: test.InventoryQueue,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			runner := roundhouse.RunnerFunc(
				func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{ExitCode: 0}, nil
				},
			)
			d := New(run.NewMemStore(), runner, nil, WithInventories(invStore))
			defer d.Close()

			var opts []run.SubmitOption
			if test.RunQueue != "" {
				opts = append(opts, run.WithQueue(test.RunQueue))
			}
			if test.InventoryID != "" {
				opts = append(opts, run.WithInventory(test.InventoryID))
			}
			created, err := d.Submit(context.Background(), "play.yml", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if created.Queue != test.WantQueue {
				t.Errorf("Queue = %q, want %q", created.Queue, test.WantQueue)
			}
		})
	}
}
