package pgstore_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/attentiontest"
	"github.com/kordloom/switchtender/internal/pgstore"
)

// TestAttentionStoreContract runs the shared worker report and alert contract against PostgreSQL.
func TestAttentionStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	attentiontest.Contract(t, func() attention.Store {
		truncateTable(t, dsn, "worker_presence")
		truncateTable(t, dsn, "attention_alerts")
		return db.Attention()
	})
}
