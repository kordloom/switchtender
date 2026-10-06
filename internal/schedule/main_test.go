package schedule_test

import (
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestMain runs the package under a Team license, because the tests that run a scheduler on
// PostgreSQL initialize a brand-new schema, which is the licensed act. Nothing in the schedule
// package reads the license, so the tier changes nothing else here.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_schedule_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	os.Exit(m.Run())
}
