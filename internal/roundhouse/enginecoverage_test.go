package roundhouse

import (
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
var enginesCIMustRun = []string{"ansible", "bash", "python", "go"}

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
		if _, err := exec.LookPath(binary); err != nil {
			t.Errorf("%s (%s) is not runnable, so its execution test skipped rather than ran. "+
				"This engine is advertised and CI installs it, so a skip here is lost coverage on "+
				"a headline claim, not an environment quirk.", want, binary)
		}
	}
}
