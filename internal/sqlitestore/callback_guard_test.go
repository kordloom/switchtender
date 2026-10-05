package sqlitestore

import (
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/storetest"
)

// TestCallbackGuardsCrossReplicas opens two handles on one database file, the way two processes
// open one SQLite install, and proves the callback lane and the budgets are held by the file rather
// than by either handle.
func TestCallbackGuardsCrossReplicas(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "st.db")
	a, err := Open(path)
	if err != nil {
		t.Fatalf("open handle a: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(path)
	if err != nil {
		t.Fatalf("open handle b: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	storetest.CallbackGuardsCrossReplicas(t, a.Runs(), b.Runs())
}
