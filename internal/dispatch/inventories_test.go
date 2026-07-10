package dispatch

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
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

			got, err := d.inventoryContent(inv)
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
	path, cleanup, err := d.inventoryFile("inv_1")
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
