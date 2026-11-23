package dispatch

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAStoredInventoryIsMarkedStaged pins that the file a stored inventory is written to is marked as
// one the dispatcher staged. The container runner judges an unmarked inventory as a path the run
// named, which gets no pass inside the run-files root, so without the mark every image run from a
// stored inventory would be refused.
func TestAStoredInventoryIsMarkedStaged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})
	d := New(run.NewMemStore(), runner, zap.NewNop(), WithRunFilesRoot(root), WithNoJanitor())
	t.Cleanup(d.Close)
	var spec roundhouse.Spec
	remove, _, err := d.materializeSnapshot(&run.Run{ID: "run_staged"}, "[web]\nweb1\n", &spec)
	if err != nil {
		t.Fatalf("materializeSnapshot() error = %v", err)
	}
	t.Cleanup(remove)
	if !spec.InventoryStaged {
		t.Error("InventoryStaged = false, want true for a stored inventory's file")
	}
	if rel, err := filepath.Rel(root, spec.Inventory); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("Inventory = %q, want a file under the run-files root %q", spec.Inventory, root)
	}
}
