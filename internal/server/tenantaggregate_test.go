package server

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestInstallWideTotalsStayInsideTheTenantBoundary covers the one number that carried another
// organization's volume across it.
//
// The rule used to be that install-wide totals are withheld from a caller who can read NOTHING.
// A viewer scoped to one organization reads something, so the test passed and the leak stayed: the
// run list handed them counts covering every organization on the install, and a Prometheus scrape
// did the same with run counts and durations. One aggregate is enough to publish a tenant's volume.
//
// The fix is not to blank the cards, which a separate test rightly refuses: a restricted caller now
// gets counts over the runs they can actually see, labeled as such, and an unrestricted caller still
// gets the true install totals.
func TestInstallWideTotalsStayInsideTheTenantBoundary(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	saveRun(t, store, "run_1", "proj_solo", base)
	saveRun(t, store, "run_2", "proj_b", base.Add(time.Second))

	// A member of one organization, under enforced grants.
	authz := orgOwnedFixture(t)(true)
	resp := listRunsFor(t, store, authz, "user_member_a", "")

	if resp.Summary.Scope == "install" {
		t.Error("a grants-restricted caller was given install-wide totals, which is every other " +
			"organization's volume in one number")
	}
	if len(resp.Runs) > 0 && resp.Summary.Total == 0 {
		t.Error("a caller who can read runs got blank summary cards, which over-corrects the leak")
	}
	if len(resp.Runs) > 0 && resp.Summary.Scope != "visible" {
		t.Errorf("summary scope = %q, want visible so the interface can say the counts cover what "+
			"this caller sees rather than calling a subset a total", resp.Summary.Scope)
	}
	// The counts must describe the rows actually returned, or the label is a second lie.
	if resp.Summary.Scope == "visible" && resp.Summary.Total != len(resp.Runs) {
		t.Errorf("visible total = %d, want %d, the runs on the page",
			resp.Summary.Total, len(resp.Runs))
	}
}
