package pgstore

import (
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestMain runs the package under a Team license, because every test here initializes a brand-new
// PostgreSQL schema, which is exactly the licensed act. The gate itself is proven in the license
// package and by TestOpenRefusesANewSchemaOnCommunity below, which swaps the license out around
// itself.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	os.Exit(m.Run())
}
