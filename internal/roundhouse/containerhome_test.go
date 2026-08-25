package roundhouse

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// TestAContainerizedRunGetsAWritableHome pins that a run told to execute as the host's uid is also
// given a home directory it can write.
//
// The two go together. Running as the host uid is what lets the container read the 0600 credential
// files and write the events sidecar, and it is also what removes the home: the uid belongs to the
// host and has no passwd entry in somebody else's image, so the runtime sets HOME to "/" and the
// container's root filesystem refuses the write. Ansible creates $HOME/.ansible/tmp before it runs
// anything and exits 5 with a permission error, so every containerized play failed on an image that
// does not happen to carry this uid, which is nearly all of them.
func TestAContainerizedRunGetsAWritableHome(t *testing.T) {
	t.Parallel()
	if os.Getuid() < 0 {
		t.Skip("no uid on this platform, so no --user and no home to give")
	}
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	spec := Spec{Tool: "ansible", Dir: "/srv/checkout", Playbook: "/srv/checkout/site.yml",
		Image: "alpine"}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		cleanup()
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	args, err := c.runArgs(spec, plan, "st-test", "")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	i := slices.Index(args, "HOME="+containerHome)
	if i == -1 {
		t.Fatalf("no writable HOME was set for a run executing as the host uid: %v", args)
	}
	if i == 0 || args[i-1] != "--env" {
		t.Errorf("HOME is not passed as an --env value: %v", args)
	}
}

// TestACallerSuppliedHomeIsKept pins that the default does not overwrite a home the caller chose.
func TestACallerSuppliedHomeIsKept(t *testing.T) {
	t.Parallel()
	if os.Getuid() < 0 {
		t.Skip("no uid on this platform")
	}
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	spec := Spec{Tool: "ansible", Dir: "/srv/checkout", Playbook: "/srv/checkout/site.yml",
		Image: "alpine", Env: []string{"HOME=/home/runner"}}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		cleanup()
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	args, err := c.runArgs(spec, plan, "st-test", "")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	if slices.Contains(args, "HOME="+containerHome) {
		t.Errorf("the default home overwrote the caller's own: %v", args)
	}
}

// TestHasEnvNameMatchesWholeNames pins that the check for an existing assignment matches the name
// and not a substring of one, so HOMEBREW_X does not read as HOME.
func TestHasEnvNameMatchesWholeNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Env  []string
		Name string
		Want bool
	}{ // Test 0: An exact assignment.
		{[]string{"HOME=/x"}, "HOME", true},
		// Test 1: A longer name that starts with it.
		{[]string{"HOMEBREW_PREFIX=/opt"}, "HOME", false},
		// Test 2: A value that contains it.
		{[]string{"PATH=/HOME=/x"}, "HOME", false},
		// Test 3: An entry with no separator at all.
		{[]string{"HOME"}, "HOME", false},
		// Test 4: Empty assignment still counts as set.
		{[]string{"HOME="}, "HOME", true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := hasEnvName(test.Env, test.Name); got != test.Want {
				t.Errorf("hasEnvName(%q, %q) = %v, want %v", test.Env, test.Name, got, test.Want)
			}
		})
	}
}
