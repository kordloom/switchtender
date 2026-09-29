package main

import "testing"

// TestNoBatteryRunsOverAnEmptySet keeps the invariant batteries from reporting green for having
// asked nothing.
//
// Every check in the invariant phase iterates one of these lists and records a pass once nothing in
// it misbehaved. Over an empty list that pass is free, and it reads in the report exactly like a
// pass that was earned. The same shape, a proof that stopped proving while still reporting, already
// cost the upgrade phase: it started from the version it was upgrading to and called that an
// upgrade. A list is cheap to empty by accident and expensive to notice.
func TestNoBatteryRunsOverAnEmptySet(t *testing.T) {
	t.Parallel()
	if len(invariantPaths) == 0 {
		t.Error("invariantPaths is empty, so every read invariant passes without reading anything")
	}
	if len(fleetHosts) == 0 {
		t.Error("fleetHosts is empty, so every fleet check passes without touching a machine")
	}
}
