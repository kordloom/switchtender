package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/forgelinktest"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestForgeLinkStoreContract runs the forge link contract against SQLite.
func TestForgeLinkStoreContract(t *testing.T) {
	t.Parallel()
	forgelinktest.Contract(t, func() forgelink.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.ForgeLinks()
	})
}
