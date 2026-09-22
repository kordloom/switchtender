package evidence

import (
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestMain runs this package under a Team license.
//
// The emitter checks the license on every tick, because the register is a paid feature and the
// startup gate runs once while this loop outlives it. These tests are about cadence, resume, and
// pack contents rather than licensing, so they need a license that covers the register. The refusal
// itself is proven by TestALapsedRegisterStopsWritingPacks, which drops the license around itself.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	os.Exit(m.Run())
}
