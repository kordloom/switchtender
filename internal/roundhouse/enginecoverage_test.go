package roundhouse

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// advertisedEngines are the seven tools the product's headline claim names. Each has an execution
// test beside this one, and each of those skips when its binary is absent.
var advertisedEngines = map[string]string{
	"ansible":    "ansible-playbook",
	"bash":       "bash",
	"python":     "python3",
	"go":         "go",
	"powershell": "pwsh",
	"terraform":  "terraform",
	"opentofu":   "tofu",
}

// enginesCIMustRun are the engines the CI environment installs and therefore must actually execute.
// A skip here is a silent loss of coverage on a headline claim, which is the failure mode this file
// exists to prevent.
// Every advertised engine is here now. Three of them were absent for the life of this list, so the
// contract that holds a tool to what its runner believes skipped for those three and reported a pass,
// which is the failure this file was written to prevent and did not.
var enginesCIMustRun = []string{
	"ansible", "bash", "python", "go", "terraform", "opentofu", "powershell",
}

// TestEveryAdvertisedEngineIsExercisedOrNamed makes an absent engine visible.
//
// Every runner has an execution test, and each of those calls t.Skip when its binary is missing. A
// skipped test reports as a pass, so an environment without pwsh or terraform ran five of the seven
// engines and said nothing about the other two. Three adversarial hunts all read these runners and
// none noticed, because the suite was green either way.
//
// This does not install anything or make the suite stricter than the machine allows. It states which
// engines this environment can prove and which it cannot, in the output, every run; and it fails when
// an engine CI does install has stopped being executable, which is a real regression wearing a skip.
func TestEveryAdvertisedEngineIsExercisedOrNamed(t *testing.T) {
	t.Parallel()
	var present, absent []string
	for tool, binary := range advertisedEngines {
		if _, err := exec.LookPath(binary); err == nil {
			present = append(present, tool)
			continue
		}
		absent = append(absent, tool+" ("+binary+")")
	}
	t.Logf("engines this environment executes: %s", strings.Join(present, ", "))
	if len(absent) > 0 {
		t.Logf("engines NOT executed here, so their runner tests skipped: %s",
			strings.Join(absent, ", "))
	}

	for _, want := range enginesCIMustRun {
		binary := advertisedEngines[want]
		if _, err := exec.LookPath(binary); err != nil && mustBeRunnableHere(want) {
			t.Errorf("%s (%s) is not runnable, so its execution test skipped rather than ran. "+
				"This engine is advertised and CI installs it, so a skip here is lost coverage on "+
				"a headline claim, not an environment quirk.", want, binary)
		}
	}
}

// mustBeRunnableHere reports whether an absent engine is a failure in this environment rather than a
// gap this environment states and moves past.
//
// One owner, because two tests ask it. This one checks that every engine CI installs is executable,
// and the tool contract beside it checks that each engine's tool behaves as its runner believes. Both
// have to treat a developer's machine and the release gate differently, and a rule written twice is a
// rule that ends up meaning two things: extending the must-run list to the last three engines made
// this test fail on any machine without PowerShell while the contract correctly skipped.
//
// The gate sets SWITCHTENDER_REQUIRE_FULL_SUITE, which is the same switch the PostgreSQL contract and
// the drift tests already read for exactly this purpose.
func mustBeRunnableHere(engine string) bool {
	if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") != "1" {
		return false
	}
	for _, want := range enginesCIMustRun {
		if want == engine {
			return true
		}
	}
	return false
}
