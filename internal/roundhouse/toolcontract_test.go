package roundhouse

import (
	"os/exec"
	"testing"

	"github.com/kordloom/switchtender/internal/enginetest"
)

// TestEveryAdvertisedEngineHasRecordedAssumptions makes an unexamined engine visible.
//
// The runners rest on facts about tools this project does not own: an exit code, a parse flag, a plan
// mode. An engine whose facts were never written down is an engine whose runner is correct by
// assertion, and the unit tests beside it point at stand-in binaries that assert the same thing back.
func TestEveryAdvertisedEngineHasRecordedAssumptions(t *testing.T) {
	t.Parallel()
	for engine := range advertisedEngines {
		if len(enginetest.Assumptions(engine)) == 0 {
			t.Errorf("the %s engine is advertised and no assumption about its tool is recorded, so "+
				"nothing states what its runner believes or checks whether that is true", engine)
		}
	}
}

// TestTheToolsBehaveAsTheRunnersAssume holds the real tools to what the runners believe about them.
//
// This is the half a stand-in cannot supply. The unit tests prove each runner builds the right
// command line and reads the result correctly, against a stub that behaves as the runner expects. It
// is the same expectation on both sides, so the pair agrees whether or not the expectation is true of
// the tool.
//
// The sharpest case is drift. The terraform runner turns exit 2 from plan -detailed-exitcode into
// Drift true and anything else into no drift. If that stopped being 2, a drifted estate would be
// reported as clean, the stub would keep agreeing, and the suite would stay green. That failure is a
// silent negative on a paid feature, which is the worst shape a defect can take here.
//
// An absent tool is named rather than skipped in silence, and an engine CI installs must run: a skip
// reports as a pass, and this file exists because a pass that proved nothing is what went unnoticed.
func TestTheToolsBehaveAsTheRunnersAssume(t *testing.T) {
	t.Parallel()
	for engine, binary := range advertisedEngines {
		t.Run(engine, func(t *testing.T) {
			t.Parallel()
			path, err := exec.LookPath(binary)
			if err != nil {
				// The same rule the PostgreSQL contract and the drift tests already follow: a
				// developer's machine is allowed to lack a tool and says so, and the gate that is
				// expected to provide everything turns that skip into a failure. Without the second
				// half a gate quietly proves less than it claims, which is how three of these
				// engines went unchecked for the life of the list.
				if mustBeRunnableHere(engine) {
					t.Fatalf("SWITCHTENDER_REQUIRE_FULL_SUITE is set and %s is not installed, so the "+
						"%s engine's assumptions about its tool went unchecked: %v",
						binary, engine, err)
				}
				t.Skipf("%s is absent here, so the %s engine's assumptions are unchecked in this "+
					"environment", binary, engine)
			}
			enginetest.Contract(t, engine, path)
		})
	}
}
