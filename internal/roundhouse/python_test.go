package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

func TestPythonRunner(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	tests := []struct {
		Command      string
		WantContains string
		WantAbsent   string
		Want         error
		WantExit     int
		DryRun       bool
	}{
		{ // Test 0: A script runs and streams its output.
			Command: "print('hello-py')", WantExit: 0, WantContains: "hello-py",
		},
		{ // Test 1: An explicit exit code is reported.
			Command: "import sys; sys.exit(4)", WantExit: 4,
		},
		{ // Test 2: A dry run compiles the script but does not execute it.
			Command: "print('SHOULD_NOT_RUN')", DryRun: true, WantExit: 0, WantAbsent: "SHOULD_NOT_RUN",
		},
		{ // Test 3: A dry run still catches a syntax error.
			Command: "def (", DryRun: true, WantExit: 1,
		},
		{ // Test 4: An empty command is rejected.
			Command: "", Want: ErrNoCommand, WantExit: -1,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			res, err := newPythonRunner(nil).Run(context.Background(),
				Spec{Tool: run.ToolPython, Command: test.Command, DryRun: test.DryRun}, &buf)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Run() error = %v, want %v", err, test.Want)
			}
			if res.ExitCode != test.WantExit {
				t.Errorf("exit = %d, want %d", res.ExitCode, test.WantExit)
			}
			if test.WantContains != "" && !strings.Contains(buf.String(), test.WantContains) {
				t.Errorf("output %q does not contain %q", buf.String(), test.WantContains)
			}
			if test.WantAbsent != "" && strings.Contains(buf.String(), test.WantAbsent) {
				t.Errorf("output %q should not contain %q", buf.String(), test.WantAbsent)
			}
		})
	}
}

func TestToolRouterPython(t *testing.T) {
	t.Parallel()
	// An empty command reaching ErrNoCommand proves the router dispatched to the python runner.
	if _, err := NewAnsibleRunner().Run(context.Background(),
		Spec{Tool: run.ToolPython, Command: ""}, io.Discard); !errors.Is(err, ErrNoCommand) {
		t.Errorf("python route error = %v, want ErrNoCommand", err)
	}
}

func TestVarsEnv(t *testing.T) {
	t.Parallel()
	env := varsEnv([]string{"BASE=1"}, Spec{
		Env:       []string{"SECRET=x"},
		ExtraVars: map[string]any{"region": "us-east-1"},
	})
	if !slices.Contains(env, "BASE=1") || !slices.Contains(env, "SECRET=x") {
		t.Errorf("env = %v, want base and credential entries", env)
	}
	if !slices.Contains(env, `SWITCHTENDER_VARS={"region":"us-east-1"}`) {
		t.Errorf("env = %v, want SWITCHTENDER_VARS with the extra vars", env)
	}
}

// TestToolRouterNewTools proves the router carries opentofu on the tofu binary and dispatches
// powershell to the pwsh runner. Routing is proven without either binary installed: opentofu by
// inspecting the router's runner, powershell by the ErrNoCommand short circuit.
func TestToolRouterNewTools(t *testing.T) {
	t.Parallel()
	router := newToolRouter(false, "docker", "missing", false, DefaultContainerLimits())
	if router.opentofu.binary != "tofu" {
		t.Errorf("opentofu binary = %q, want tofu", router.opentofu.binary)
	}
	if router.powershell.binary != "pwsh" {
		t.Errorf("powershell binary = %q, want pwsh", router.powershell.binary)
	}
	if _, err := router.Run(context.Background(),
		Spec{Tool: run.ToolPowerShell, Command: ""}, io.Discard); !errors.Is(err, ErrNoCommand) {
		t.Errorf("powershell route error = %v, want ErrNoCommand", err)
	}
}
