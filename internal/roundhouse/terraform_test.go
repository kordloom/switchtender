package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/railwarden/internal/run"
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
	if got := buf.String(); !strings.Contains(got, "plan") || strings.Contains(got, "apply") {
		t.Errorf("dry-run output = %q, want plan and no apply", got)
	}

	if _, err := r.Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Command: ""}, io.Discard); !errors.Is(err, ErrNoCommand) {
		t.Errorf("empty command error = %v, want ErrNoCommand", err)
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
