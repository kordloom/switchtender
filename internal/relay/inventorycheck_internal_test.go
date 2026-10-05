package relay

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAWorkerReportCarriesTheInventoryCheck pins that the cross-check a worker made before its play
// reaches the run the control node holds, which the outcome record commits. The worker learned it
// by executing, like the commit it checked out, so it is the worker's to report.
func TestAWorkerReportCarriesTheInventoryCheck(t *testing.T) {
	t.Parallel()
	stored := &run.Run{ID: "r1", Status: run.StatusRunning, ClaimedBy: "worker-a"}
	reported := &run.Run{ID: "r1", Status: run.StatusSucceeded,
		InventoryCheck: &run.InventoryCheck{AnsibleCore: "2.18.1", ResolvedDigest: "sha256:bb",
			Differences: []string{"none"}}}
	applyWorkerReport(stored, reported)
	want := reported.InventoryCheck.Clone()
	reported.InventoryCheck.Differences[0] = "changed after the report"
	if diff := cmp.Diff(want, stored.InventoryCheck); diff != "" {
		t.Errorf("stored inventory check mismatch (-want +got):\n%s", diff)
	}
}
