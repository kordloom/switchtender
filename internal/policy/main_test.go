package policy

import (
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestMain runs this package under a Team licence.
//
// The file store checks the licence on every read, because the file hot-reloads and a startup-only
// check was one edit away from not existing. These tests are about parsing and reload behavior, so
// they need a licence that covers the rules they parse. The refusal itself is proven by
// TestFileStoreRefusesRulesTheLicenceDoesNotCover, which drops the licence around itself.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	os.Exit(m.Run())
}
