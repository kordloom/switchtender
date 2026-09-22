package dispatch

import (
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestMain runs this package under a Team license.
//
// A run pinned to a named queue is refused on an install that cannot run a worker to serve it,
// because nothing could ever claim it. Most tests here are about scheduling, leases, splits and
// policy rather than licensing, and several of them pin work to a queue to say something about
// claiming, so they need a license that covers the queues they use. The refusal itself is proven
// by TestAQueueInheritedFromAnInventoryIsLicensed, which drops the license around itself.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	os.Exit(m.Run())
}
