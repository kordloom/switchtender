package dispatch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// dumpRecorder is a Runner that also dumps inventories, recording the path it was asked to dump.
//
// The dumper half is what makes the refresh path reachable at all: New type-asserts the runner to
// roundhouse.InventoryDumper, and a plain RunnerFunc leaves the dispatcher without one, so
// RefreshSource returns ErrNotFound before it reaches any guard.
type dumpRecorder struct {
	// dumped holds every source path handed to Dump.
	dumped []string
}

// Run satisfies roundhouse.Runner and is never expected to be called here.
func (d *dumpRecorder) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 0}, nil
}

// Dump records the path and returns an empty but parseable inventory.
func (d *dumpRecorder) Dump(_ context.Context, source string, _ []string) ([]byte, error) {
	d.dumped = append(d.dumped, source)
	return []byte(`{"_meta":{"hostvars":{}},"all":{"children":[]}}`), nil
}

// TestRefreshRefusesADangerousBareSource drives RefreshSource, the only path that reaches the bare
// source guard, and confirms a hostile path is refused before the dumper ever sees it.
//
// The guard had a direct unit test and nothing executed its call site, so removing the call left the
// suite green. That gap mattered more here than in most places: a bare source is handed straight to
// ansible-inventory, which EXECUTES it when it is an executable file, so a stored source pointing at
// one is arbitrary code execution as the executor. The dump recorder is what makes the assertion
// real, since a test that checked only the returned error would pass even if the file had already
// been run.
func TestRefreshRefusesADangerousBareSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	executable := filepath.Join(dir, "hosts.sh")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	plain := filepath.Join(dir, "hosts.ini")
	if err := os.WriteFile(plain, []byte("[web]\nweb01\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tests := []struct {
		Name    string
		Source  string
		Refused bool
	}{
		{Name: "an executable file would be run by ansible-inventory", Source: executable, Refused: true},
		// Built by concatenation, not filepath.Join, which would clean the ".." away before the
		// guard ever saw it and leave this case asserting nothing.
		{Name: "a traversing path", Source: dir + "/../escape.ini", Refused: true},
		{Name: "an empty path", Source: "", Refused: true},
		{Name: "a plain readable file is allowed", Source: plain},
		{
			Name:   "a path that does not exist is left for ansible-inventory to report",
			Source: filepath.Join(dir, "absent.ini"),
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			sources := invsource.NewMemStore()
			if err := sources.Save(ctx, &invsource.Source{
				ID: "src_1", Name: "bare", Source: test.Source, InventoryID: "inv_1",
			}); err != nil {
				t.Fatalf("Save(source) error = %v", err)
			}
			inventories := inventory.NewMemStore()
			if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "prod"}); err != nil {
				t.Fatalf("Save(inventory) error = %v", err)
			}

			runner := &dumpRecorder{}
			d := New(run.NewMemStore(), runner, zap.NewNop(),
				WithInventorySources(sources), WithInventories(inventories))
			defer d.Close()

			src, err := d.RefreshSource(ctx, "src_1")

			if !test.Refused {
				if err != nil {
					t.Fatalf("RefreshSource() error = %v, want the source to refresh", err)
				}
				if len(runner.dumped) != 1 {
					t.Errorf("dumper ran %d time(s), want 1", len(runner.dumped))
				}
				return
			}

			if !errors.Is(err, invsource.ErrInvalidSource) {
				t.Errorf("RefreshSource() error = %v, want ErrInvalidSource", err)
			}
			// The refusal has to happen before the dump. ansible-inventory executes an executable
			// source, so reporting the error afterward would report a command that already ran.
			if len(runner.dumped) != 0 {
				t.Errorf("the source was dumped anyway as %v; an executable source is executed by "+
					"ansible-inventory, so this is code execution as the executor", runner.dumped)
			}
			// The failure is recorded on the source so a refused source is visible rather than
			// silently never syncing.
			if src == nil || src.LastError == "" {
				t.Error("the refusal was not recorded on the source, so it looks like it never ran")
			}
		})
	}
}
