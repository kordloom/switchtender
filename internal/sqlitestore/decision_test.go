package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/decisiontest"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestDecisionStoreContract runs the decision record contract against SQLite.
func TestDecisionStoreContract(t *testing.T) {
	t.Parallel()
	decisiontest.Contract(t, func() decision.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Decisions()
	})
}
