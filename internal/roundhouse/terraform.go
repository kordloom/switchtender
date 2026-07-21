package roundhouse

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// terraformRunner runs Terraform in a working directory as child processes. The Spec's Command names
// the directory, relative to the project checkout, that holds the .tf files. It runs terraform init
// then apply, or plan for a dry run, so a dry run previews changes without touching infrastructure.
// Extra vars flow in as TF_VAR_ environment entries, and materialized credentials arrive in Env.
type terraformRunner struct {
	// binary is the terraform executable name or path.
	binary string
	// baseEnv is the environment inherited by every execution.
	baseEnv []string
}

// newTerraformRunner returns a terraformRunner that resolves terraform from PATH and inherits baseEnv.
func newTerraformRunner(baseEnv []string) *terraformRunner {
	return &terraformRunner{binary: "terraform", baseEnv: baseEnv}
}

// Run initializes and applies the Terraform working directory, streaming combined output to out. A
// dry run runs plan instead of apply, so nothing is changed. init runs first; if it fails the run
// stops there with init's result.
func (t *terraformRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Command == "" {
		return Result{ExitCode: -1}, ErrNoCommand
	}
	dir, err := toolWorkDir(spec.Dir, spec.Command)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	env := append(append([]string{}, t.baseEnv...), spec.Env...)
	env = append(env, terraformVars(spec.ExtraVars)...)

	initCmd := exec.CommandContext(ctx, t.binary, "init", "-input=false", "-no-color")
	initCmd.Dir = dir
	initCmd.Env = env
	if res, err := runProcess(ctx, initCmd, out); err != nil || res.ExitCode != 0 {
		return res, err
	}

	args := []string{"apply", "-auto-approve", "-input=false", "-no-color"}
	if spec.DryRun {
		// -detailed-exitcode makes plan return 2 when there are pending changes, so a dry run can
		// tell a clean state from a drifted one instead of always exiting 0.
		args = []string{"plan", "-input=false", "-no-color", "-detailed-exitcode"}
	}
	cmd := exec.CommandContext(ctx, t.binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	res, err := runProcess(ctx, cmd, out)
	if spec.DryRun && err == nil && res.ExitCode == 2 {
		// Exit 2 is a successful plan with pending changes, which is drift, not a failure. Report it
		// as success with the drift flag so the run is not marked failed.
		return Result{ExitCode: 0, Drift: true}, nil
	}
	return res, err
}

// terraformVars renders extra vars as TF_VAR_ environment entries so survey answers and template
// vars flow into Terraform. Scalars pass through as strings; complex values are JSON, which
// Terraform accepts for list and map variables. Entries are sorted for a deterministic environment.
func terraformVars(vars map[string]any) []string {
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		if s, ok := v.(string); ok {
			out = append(out, "TF_VAR_"+k+"="+s)
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out = append(out, "TF_VAR_"+k+"="+string(b))
	}
	sort.Strings(out)
	return out
}

// toolWorkDir resolves a tool's working directory from a base checkout and a subdirectory, rejecting
// a subdirectory that escapes the base with .. so a run cannot reach outside its project. With no
// base the subdirectory is used as given, relative to the process working directory.
func toolWorkDir(base, sub string) (string, error) {
	if base == "" {
		return sub, nil
	}
	joined := filepath.Join(base, sub)
	rel, err := filepath.Rel(base, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrBadWorkDir
	}
	return joined, nil
}
