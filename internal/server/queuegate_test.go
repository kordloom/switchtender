package server

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestANamedQueueIsRefusedWithoutAWorkerLicense covers a field the product sells as Team and did not
// gate.
//
// A queue restricts a run to workers serving that name, and every worker is Team. On Community the
// field saved cleanly, the run was accepted with 202, and then nothing could ever claim it: it sat
// pending forever with no error on any surface to say why. Every other gate in this product refuses
// and names the tier; this one produced a silently stranded run, which is the worst shape a gate can
// take because the operator has no way to learn what happened.
//
// Not parallel, and it goes through asCommunity: the license is process state, and this package runs
// under a Team license from TestMain.
func TestANamedQueueIsRefusedWithoutAWorkerLicense(t *testing.T) {
	// Under Team, which is what TestMain installs, a named queue is exactly what was bought.
	if err := allowQueue("prod"); err != nil {
		t.Errorf("a Team install was refused its own queue: %v", err)
	}

	asCommunity(t, func() {
		// The default queue is the server's own pool and needs no worker, at any tier.
		for _, free := range []string{"", "   "} {
			if err := allowQueue(free); err != nil {
				t.Errorf("allowQueue(%q) = %v, want allowed: the default pool needs no worker",
					free, err)
			}
		}
		err := allowQueue("prod")
		if err == nil {
			t.Fatal("a Community install accepted a named queue, so the run it routes can never " +
				"be claimed and sits pending with nothing to explain it")
		}
		// A refusal has to name the tier and where to go, the same as every other gate, rather than
		// leaving the operator to discover a run that never ran.
		if !strings.Contains(err.Error(), "switchtender.com/pricing") {
			t.Errorf("refusal does not point anywhere: %v", err)
		}
		if !strings.Contains(err.Error(), string(license.FeatureWorkers)) &&
			!strings.Contains(strings.ToLower(err.Error()), "worker") {
			t.Errorf("refusal does not name the feature: %v", err)
		}
	})
}
