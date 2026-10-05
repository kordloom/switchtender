package roundhouse

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestTerraformNeverCallsHomeByDefault holds every Terraform and OpenTofu process the runner starts
// to the no-phone-home promise. Each command makes a version check against the vendor's checkpoint
// service unless CHECKPOINT_DISABLE is set, so the runner sets it on the init, the plan or apply,
// and the module download, unless the run's own environment sets it, which stands as the run's
// choice. A stub stands in for the tool and records each command with the value it saw.
//
//nolint:funlen // Test function.
func TestTerraformNeverCallsHomeByDefault(t *testing.T) {
	t.Parallel()
	stub := filepath.Join(t.TempDir(), "tf")
	writeStub(t, stub, "#!/bin/sh\n[ -n \"$OUT\" ] && printf '%s|%s\\n' \"$*\" "+
		"\"${CHECKPOINT_DISABLE-unset}\" >> \"$OUT\"\nexit 0\n")
	tests := []struct {
		// Env is the run's own environment.
		Env []string
		// Fetch runs the module download rather than the plan.
		Fetch bool
		// Installed says the modules the run must use are already in place.
		Installed bool
		// WantLines are the commands the stub saw, each with the value it saw.
		WantLines []string
	}{{ // Test 0: A plan turns the check off for init and plan.
		WantLines: []string{"init -input=false -no-color|1",
			"plan -input=false -no-color -detailed-exitcode|1"},
	}, { // Test 1: A run that sets the variable keeps its own value, even an empty one.
		Env: []string{"CHECKPOINT_DISABLE="},
		WantLines: []string{"init -input=false -no-color|",
			"plan -input=false -no-color -detailed-exitcode|"},
	}, { // Test 2: The module download turns it off too.
		Fetch: true, WantLines: []string{"get -no-color|1"},
	}, { // Test 3: Over modules the gate read, init installs none.
		Installed: true,
		WantLines: []string{"init -input=false -no-color -get=false|1",
			"plan -input=false -no-color -detailed-exitcode|1"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			out := filepath.Join(t.TempDir(), "seen")
			spec := Spec{Tool: run.ToolTerraform, Command: t.TempDir(), DryRun: true,
				ModulesInstalled: test.Installed, Env: append([]string{"OUT=" + out}, test.Env...)}
			r := &terraformRunner{binary: stub}
			var err error
			if test.Fetch {
				_, err = r.fetchModules(context.Background(), spec, io.Discard)
			} else {
				_, err = r.Run(context.Background(), spec, io.Discard)
			}
			if err != nil {
				t.Fatalf("run error = %v", err)
			}
			seen, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read what the stub saw: %v", err)
			}
			got := strings.Split(strings.TrimSpace(string(seen)), "\n")
			if diff := cmp.Diff(test.WantLines, got); diff != "" {
				t.Errorf("commands mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// The container forms carry the same entry in the environment the container reads.
	for _, build := range []func(Spec) (containerPlan, func(), error){buildContainerPlan,
		buildModulesPlan} {
		plan, cleanup, err := build(Spec{Tool: run.ToolOpenTofu, Command: ".", Dir: t.TempDir(),
			Image: "registry.example.test/tf:1"})
		cleanup()
		if err != nil {
			t.Fatalf("build container plan: %v", err)
		}
		if !hasEnvName(plan.extraEnv, checkpointVar) {
			t.Errorf("container plan %q carries %q, want CHECKPOINT_DISABLE=1", plan.argv, plan.extraEnv)
		}
		own, ocleanup, err := build(Spec{Tool: run.ToolOpenTofu, Command: ".", Dir: t.TempDir(),
			Image: "registry.example.test/tf:1", Env: []string{"CHECKPOINT_DISABLE="}})
		ocleanup()
		if err != nil {
			t.Fatalf("build container plan: %v", err)
		}
		if hasEnvName(own.extraEnv, checkpointVar) {
			t.Errorf("container plan %q overrides the run's own CHECKPOINT_DISABLE: %q", own.argv,
				own.extraEnv)
		}
	}
}
