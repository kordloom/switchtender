package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

func TestTerraformRunnerArgs(t *testing.T) {
	t.Parallel()
	// echo stands in for terraform so the test captures the argv without needing terraform installed.
	r := &terraformRunner{binary: "echo"}

	var buf bytes.Buffer
	res, err := r.Run(context.Background(), Spec{Tool: run.ToolTerraform, Command: "."}, &buf)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("apply: exit=%d err=%v", res.ExitCode, err)
	}
	if got := buf.String(); !strings.Contains(got, "init") || !strings.Contains(got, "apply -auto-approve") {
		t.Errorf("apply output = %q, want init then apply", got)
	}

	buf.Reset()
	if _, err := r.Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Command: ".", DryRun: true}, &buf); err != nil {
		t.Fatalf("plan: err=%v", err)
	}
	if got := buf.String(); !strings.Contains(got, "plan") ||
		!strings.Contains(got, "-detailed-exitcode") || strings.Contains(got, "apply") {
		t.Errorf("dry-run output = %q, want plan with -detailed-exitcode and no apply", got)
	}

	if _, err := r.Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Command: ""}, io.Discard); !errors.Is(err, ErrNoCommand) {
		t.Errorf("empty command error = %v, want ErrNoCommand", err)
	}
}

// TestTerraformRunnerDrift verifies a dry run whose plan reports pending changes is reported as a
// successful check with the drift flag set, not as a failure, from the plan's detailed exit code.
func TestTerraformRunnerDrift(t *testing.T) {
	t.Parallel()
	// A stub standing in for terraform: init succeeds, and plan exits 2, the detailed-exit-code
	// signal for pending changes.
	stub := filepath.Join(t.TempDir(), "tf")
	writeStub(t, stub, "#!/bin/sh\ncase \"$1\" in\nplan) exit 2 ;;\n*) exit 0 ;;\nesac\n")
	drifted := &terraformRunner{binary: stub}
	res, err := drifted.Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Command: ".", DryRun: true}, io.Discard)
	if err != nil {
		t.Fatalf("plan drift: err=%v", err)
	}
	if res.ExitCode != 0 || !res.Drift {
		t.Errorf("plan drift result = %+v, want exit 0 with Drift true", res)
	}

	// A clean plan exits 0 and reports no drift.
	clean := filepath.Join(t.TempDir(), "tf")
	writeStub(t, clean, "#!/bin/sh\nexit 0\n")
	res, err = (&terraformRunner{binary: clean}).Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Command: ".", DryRun: true}, io.Discard)
	if err != nil || res.ExitCode != 0 || res.Drift {
		t.Errorf("clean plan result = %+v err=%v, want exit 0 and no drift", res, err)
	}
}

func TestToolRouterTerraform(t *testing.T) {
	t.Parallel()
	// An empty command reaching ErrNoCommand proves the router dispatched to the terraform runner.
	if _, err := NewAnsibleRunner().Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Command: ""}, io.Discard); !errors.Is(err, ErrNoCommand) {
		t.Errorf("terraform route error = %v, want ErrNoCommand", err)
	}
}

func TestTerraformVars(t *testing.T) {
	t.Parallel()
	got := terraformVars(map[string]any{
		"region": "us-east-1",
		"count":  3,
		"tags":   []string{"a", "b"},
	})
	want := []string{
		`TF_VAR_count=3`,
		`TF_VAR_region=us-east-1`,
		`TF_VAR_tags=["a","b"]`,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("terraformVars mismatch (-want +got):\n%s", diff)
	}
}

func TestToolWorkDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Base    string
		Sub     string
		WantDir string
		Want    error
	}{
		{Base: "", Sub: "infra", WantDir: "infra"},
		{Base: "/proj", Sub: "infra", WantDir: "/proj/infra"},
		{Base: "/proj", Sub: ".", WantDir: "/proj"},
		{Base: "/proj", Sub: "../etc", Want: ErrBadWorkDir},
		{Base: "/proj", Sub: "a/../../etc", Want: ErrBadWorkDir},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := toolWorkDir(test.Base, test.Sub)
			if !errors.Is(err, test.Want) {
				t.Fatalf("err = %v, want %v", err, test.Want)
			}
			if test.Want == nil && got != test.WantDir {
				t.Errorf("dir = %q, want %q", got, test.WantDir)
			}
		})
	}
}
