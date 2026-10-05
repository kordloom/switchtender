package pgstore_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/forgelinktest"
	"github.com/kordloom/switchtender/internal/pgstore"
)

// TestForgeLinkStoreContract runs the forge link contract against PostgreSQL.
func TestForgeLinkStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	forgelinktest.Contract(t, func() forgelink.Store {
		truncateTable(t, dsn, "forge_links")
		return db.ForgeLinks()
	})
}
