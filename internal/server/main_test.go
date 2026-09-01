package server

import (
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestMain runs the package under a Team license, because the suite exercises Team features
// throughout: deny policies, risk floors, reconcile proposals. The refusal paths are proven by the
// gate tests, which swap the license out and back around themselves and stay sequential for it.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	os.Exit(m.Run())
}
