package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dcadolph/yardmaster/internal/run"
)

func TestBashRunner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Command      string
		WantContains string
		WantAbsent   string
		Want         error
		WantExit     int
		DryRun       bool
	}{
		{ // Test 0: A script runs and streams its output.
			Command: "echo hello-bash", WantExit: 0, WantContains: "hello-bash",
		},
		{ // Test 1: A non-zero exit is reported as that code with no error.
			Command: "exit 3", WantExit: 3,
		},
		{ // Test 2: A dry run parses the script but does not execute it.
			Command: "echo SHOULD_NOT_RUN", DryRun: true, WantExit: 0, WantAbsent: "SHOULD_NOT_RUN",
		},
		{ // Test 3: A dry run still catches a syntax error.
			Command: "if", DryRun: true, WantExit: 2,
		},
		{ // Test 4: An empty command is rejected.
			Command: "", Want: ErrNoCommand, WantExit: -1,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			res, err := newBashRunner(nil).Run(context.Background(),
				Spec{Tool: run.ToolBash, Command: test.Command, DryRun: test.DryRun}, &buf)
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

func TestToolRouterRoutes(t *testing.T) {
	t.Parallel()
	runner := NewAnsibleRunner()

	var buf bytes.Buffer
	res, err := runner.Run(context.Background(),
		Spec{Tool: run.ToolBash, Command: "echo routed"}, &buf)
	if err != nil || res.ExitCode != 0 || !strings.Contains(buf.String(), "routed") {
		t.Fatalf("bash route: exit=%d err=%v out=%q", res.ExitCode, err, buf.String())
	}

	if _, err := runner.Run(context.Background(),
		Spec{Tool: "make-believe", Command: "x"}, &buf); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("unknown tool error = %v, want ErrUnknownTool", err)
	}
}

func TestBashReceivesExtraVars(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	res, err := newBashRunner(nil).Run(context.Background(),
		Spec{Command: `echo "$YARDMASTER_VARS"`, ExtraVars: map[string]any{"region": "us-east-1"}}, &buf)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("bash run: exit=%d err=%v", res.ExitCode, err)
	}
	if !strings.Contains(buf.String(), `"region":"us-east-1"`) {
		t.Errorf("output %q, want the extra vars in YARDMASTER_VARS", buf.String())
	}
}
