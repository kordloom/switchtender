package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/dcadolph/yardmaster/internal/run"
)

func TestGoRunner(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	tests := []struct {
		Command      string
		WantContains string
		WantAbsent   string
		Want         error
		WantExit     int
		DryRun       bool
	}{
		{ // Test 0: A program runs and streams its output.
			Command:  "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello-go\") }\n",
			WantExit: 0, WantContains: "hello-go",
		},
		{ // Test 1: A failing program fails the run. go run reports its own exit status, not the program's.
			Command:  "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(5) }\n",
			WantExit: 1, WantContains: "exit status 5",
		},
		{ // Test 2: A dry run vets the source, reports a clean result, and does not execute it.
			Command: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"SHOULD_NOT_RUN\") }\n",
			DryRun:  true, WantExit: 0,
			WantContains: "go vet: no problems found", WantAbsent: "SHOULD_NOT_RUN",
		},
		{ // Test 3: A dry run still catches a compile error and its log is not empty.
			Command: "package main\n\nfunc main() {\n", DryRun: true, WantExit: 1,
			WantContains: "dry run: go vet",
		},
		{ // Test 4: An empty command is rejected.
			Command: "", Want: ErrNoCommand, WantExit: -1,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			// go run needs the toolchain environment, GOCACHE and HOME, to build, so the runner
			// inherits the process environment the way the tool router builds it in production.
			res, err := newGoRunner(os.Environ()).Run(context.Background(),
				Spec{Tool: run.ToolGo, Command: test.Command, DryRun: test.DryRun}, &buf)
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

func TestToolRouterGo(t *testing.T) {
	t.Parallel()
	// An empty command reaching ErrNoCommand proves the router dispatched to the go runner.
	if _, err := NewAnsibleRunner().Run(context.Background(),
		Spec{Tool: run.ToolGo, Command: ""}, io.Discard); !errors.Is(err, ErrNoCommand) {
		t.Errorf("go route error = %v, want ErrNoCommand", err)
	}
}
