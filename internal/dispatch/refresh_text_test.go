package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// failingDumper is a runner whose inventory dump fails with text it did not choose.
type failingDumper struct {
	// text is the dump failure's message.
	text string
}

// Run executes nothing.
func (failingDumper) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{}, nil
}

// Dump fails with the dumper's text.
func (f failingDumper) Dump(context.Context, string, []string) ([]byte, error) {
	return nil, errors.New(f.text)
}

// sourceStores are the stores a source refresh reads and writes on one backend.
type sourceStores struct {
	// sources holds the inventory sources.
	sources invsource.Store
	// inventories holds the inventories they maintain.
	inventories inventory.Store
}

// TestASourceFailureIsRecordedOnEveryBackend refreshes an inventory source whose plugin fails with
// a NUL byte and a byte that is not UTF-8 in its message, on every backend.
//
// The refresh records the failure on the source so a broken source is visible rather than silently
// stale. The text is the plugin's, and on PostgreSQL the save refused it, the refresh discarded
// that error, and the source showed its last success as though nothing had gone wrong.
func TestASourceFailureIsRecordedOnEveryBackend(t *testing.T) {
	t.Parallel()
	plain := filepath.Join(t.TempDir(), "hosts.ini")
	if err := os.WriteFile(plain, []byte("[web]\nweb01\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	backends := []struct {
		Name string
		Open func(t *testing.T) sourceStores
	}{{ // Test 0: The in-memory stores.
		Name: "memory", Open: func(*testing.T) sourceStores {
			return sourceStores{sources: invsource.NewMemStore(), inventories: inventory.NewMemStore()}
		},
	}, { // Test 1: SQLite.
		Name: "sqlite", Open: func(t *testing.T) sourceStores {
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return sourceStores{sources: db.InventorySources(), inventories: db.Inventories()}
		},
	}, { // Test 2: PostgreSQL.
		Name: "postgres", Open: func(t *testing.T) sourceStores {
			db, err := pgstore.Open(leaseFreshPostgres(t))
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return sourceStores{sources: db.InventorySources(), inventories: db.Inventories()}
		},
	}}
	for testNum, backend := range backends {
		t.Run(fmt.Sprintf("test %d %s", testNum, backend.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			stores := backend.Open(t)
			inv := &inventory.Inventory{ID: "inv_src", Name: "prod"}
			if err := stores.inventories.Save(ctx, inv); err != nil {
				t.Fatalf("save inventory: %v", err)
			}
			if err := stores.sources.Save(ctx, &invsource.Source{ID: "src_text", Name: "cloud",
				Source: plain, InventoryID: "inv_src"}); err != nil {
				t.Fatalf("save source: %v", err)
			}
			d := dispatch.New(run.NewMemStore(), failingDumper{text: "plugin said \x00 caf\xe9"},
				zap.NewNop(), dispatch.WithInventorySources(stores.sources),
				dispatch.WithInventories(stores.inventories), dispatch.WithNoJanitor(),
				dispatch.WithRunFilesRoot(t.TempDir()))
			t.Cleanup(d.Close)
			if _, err := d.RefreshSource(ctx, "src_text"); err == nil {
				t.Fatal("RefreshSource() succeeded with a failing plugin")
			}
			got, err := stores.sources.Get(ctx, "src_text")
			if err != nil {
				t.Fatalf("Get(source) error = %v", err)
			}
			if !strings.Contains(got.LastError, "plugin said � caf�") {
				t.Errorf("recorded failure = %q, want the plugin's text with its unstorable bytes "+
					"replaced", got.LastError)
			}
		})
	}
}
