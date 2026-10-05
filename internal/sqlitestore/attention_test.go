package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/attentiontest"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestAttentionStoreContract runs the shared worker report and alert contract against SQLite.
func TestAttentionStoreContract(t *testing.T) {
	t.Parallel()
	attentiontest.Contract(t, func() attention.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Attention()
	})
}
