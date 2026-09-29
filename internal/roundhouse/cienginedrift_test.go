package roundhouse

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatesRequiringEveryEngine are the workflows that run the suite with
// SWITCHTENDER_REQUIRE_FULL_SUITE set, which turns an absent engine from a skip into a failure. A
// workflow that demands an engine it does not install cannot pass, so each of these has to carry
// every engine in enginesCIMustRun. The release no longer reruns the suite: it requires ci green on
// the commit it releases, so ci is the one gate that runs it.
var gatesRequiringEveryEngine = []string{
	".github/workflows/ci.yml",
}

// TestEveryRequiredEngineIsInstalledByEveryGate stops the two halves of this rule drifting apart.
//
// Which engines CI must run is decided here, in Go. Which engines CI installs is decided in YAML,
// by hand. Nothing connected them, so adding three engines to the list on one side once left the
// release gate demanding seven and shipping one, found only when a tag was already pushed. The
// suite now runs strictly in ci on every push, and this keeps its installs honest at desk speed.
//
// Each gate names every required binary in one loop, so this reads the workflow and requires the
// name to be there. A binary absent from a gate fails here, at desk speed, instead of at release
// time.
func TestEveryRequiredEngineIsInstalledByEveryGate(t *testing.T) {
	t.Parallel()
	root, err := repoRootForTest()
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	for _, gate := range gatesRequiringEveryEngine {
		t.Run(filepath.Base(gate), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join(root, gate))
			if err != nil {
				t.Fatalf("read %s: %v", gate, err)
			}
			workflow := string(raw)
			for _, engine := range enginesCIMustRun {
				binary, ok := advertisedEngines[engine]
				if !ok {
					t.Errorf("%s is required of CI and is not an advertised engine, so nothing "+
						"says which binary it needs", engine)
					continue
				}
				if !strings.Contains(workflow, binary) {
					t.Errorf("the %s engine is required of CI and %s never names %q, so that gate "+
						"demands an engine it does not provide and the suite fails there instead "+
						"of here", engine, gate, binary)
				}
			}
		})
	}
}

// repoRootForTest walks up from the test's directory to the module root, so the test does not
// depend on where it was invoked from.
func repoRootForTest() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for range 10 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no go.mod above the test's directory")
}
