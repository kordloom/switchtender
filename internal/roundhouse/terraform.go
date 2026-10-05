package roundhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
//
// A dry run given PlanOut saves its plan there and returns the plan file and its JSON rendering. An
// apply given PlanFile carries out that saved plan instead of planning again: the tool applies
// exactly what the plan says and refuses a plan made stale by a change since. Neither the plan file
// nor its rendering is written to out, and the values the plan marks sensitive are handed to
// AddSecrets first.
func (t *terraformRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Command == "" {
		return Result{ExitCode: -1}, ErrNoCommand
	}
	dir, err := toolWorkDir(spec.Dir, spec.Command)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	// Checked here rather than left to exec. A run carrying no project has no checkout to resolve
	// against, so the working directory comes back as the command string itself, and starting a
	// process in a directory that does not exist fails with ENOENT reported against the executable.
	// The reader is then told terraform is missing while terraform is sitting on the PATH, and the
	// thing actually missing, a project for it to run in, is never mentioned.
	if info, serr := os.Stat(dir); serr != nil || !info.IsDir() {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %q, which is where %s would run. A run of "+
			"this tool needs a project whose checkout holds the directory its command names",
			ErrNoWorkDir, dir, t.binary)
	}
	env := append(append([]string{}, t.baseEnv...), spec.Env...)
	env = append(env, checkpointEnv(spec.Env)...)
	env = append(env, terraformVars(spec.ExtraVars)...)

	initCmd := exec.CommandContext(ctx, t.binary, terraformInitArgs(spec.ModulesInstalled)...)
	initCmd.Dir = dir
	initCmd.Env = env
	if res, err := runProcess(ctx, initCmd, out); err != nil || res.ExitCode != 0 {
		return res, err
	}

	if !spec.DryRun && spec.PlanFile != "" && spec.AddSecrets != nil {
		// Rendered first so the masker holds the plan's sensitive values before the apply prints
		// anything. A plan that will not render is left for the apply to refuse.
		if rendered, serr := t.show(ctx, dir, env, spec.PlanFile, out); serr == nil {
			spec.AddSecrets(PlanSensitiveValues(rendered))
		}
	}
	cmd := exec.CommandContext(ctx, t.binary, terraformActionArgs(spec)...)
	cmd.Dir = dir
	cmd.Env = env
	res, err := runProcess(ctx, cmd, out)
	if spec.DryRun && err == nil && res.ExitCode == 2 {
		// Exit 2 is a successful plan with pending changes, which is drift, not a failure. Report it
		// as success with the drift flag so the run is not marked failed.
		res = Result{ExitCode: 0, Drift: true}
	}
	if spec.DryRun && spec.PlanOut != "" && err == nil && res.ExitCode == 0 {
		t.readPlan(ctx, dir, env, spec, out, &res)
	}
	return res, err
}

// readPlan reads back the plan a dry run saved to spec.PlanOut and renders it as JSON, storing both
// on res. A plan that was not saved, or that will not render, is left off, so the caller treats it as
// a plan nobody could measure.
func (t *terraformRunner) readPlan(ctx context.Context, dir string, env []string, spec Spec,
	out io.Writer, res *Result) {
	plan, err := os.ReadFile(spec.PlanOut)
	if err != nil || len(plan) == 0 {
		return
	}
	res.PlanFile = plan
	rendered, err := t.show(ctx, dir, env, spec.PlanOut, out)
	if err != nil {
		return
	}
	if spec.AddSecrets != nil {
		spec.AddSecrets(PlanSensitiveValues(rendered))
	}
	res.PlanJSON = rendered
}

// show renders a saved plan file as JSON. Its standard output is captured and never reaches out,
// since it carries the plan's values in the clear. What the tool prints to standard error, such as
// why a plan will not render, goes to out like the rest of the run's output.
func (t *terraformRunner) show(ctx context.Context, dir string, env []string, planFile string,
	out io.Writer) ([]byte, error) {
	cmd := exec.CommandContext(ctx, t.binary, terraformShowArgs(planFile)...)
	cmd.Dir = dir
	cmd.Env = env
	var captured cappedCapture
	cmd.Stdout = &captured
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return captured.bytes()
}

// fetchModules runs the get of spec's working directory and nothing else: it downloads the modules
// the configuration calls, installs no provider, and plans nothing. It inherits the same base
// environment and credential environment Run gives the tool, and no variables.
func (t *terraformRunner) fetchModules(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Command == "" {
		return Result{ExitCode: -1}, ErrNoCommand
	}
	dir, err := toolWorkDir(spec.Dir, spec.Command)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	if info, serr := os.Stat(dir); serr != nil || !info.IsDir() {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %q, which is where %s would fetch modules",
			ErrNoWorkDir, dir, t.binary)
	}
	get := exec.CommandContext(ctx, t.binary, terraformGetArgs()...)
	get.Dir = dir
	get.Env = append(append([]string{}, t.baseEnv...), spec.Env...)
	get.Env = append(get.Env, checkpointEnv(spec.Env)...)
	return runProcess(ctx, get, out)
}

// checkpointVar is the variable that turns off the version check Terraform and OpenTofu make on
// every command, which sends the tool's version and platform to the vendor's checkpoint service.
const checkpointVar = "CHECKPOINT_DISABLE"

// checkpointEnv returns the entry that turns the version check off, so a tool this runner starts
// never calls home by default. A run whose own environment sets the variable, to any value, keeps
// its own choice, and nothing is added.
func checkpointEnv(env []string) []string {
	if hasEnvName(env, checkpointVar) {
		return nil
	}
	return []string{checkpointVar + "=1"}
}

// terraformInitArgs is the terraform init argument list, shared by the host runner and the container
// plan. When the working directory already holds the modules the run must use, init installs none,
// so it cannot resolve a module version again.
func terraformInitArgs(modulesInstalled bool) []string {
	args := []string{"init", "-input=false", "-no-color"}
	if modulesInstalled {
		args = append(args, "-get=false")
	}
	return args
}

// terraformGetArgs is the argument list of the get that downloads a working directory's modules and
// nothing else, shared by the host runner and the container plan.
func terraformGetArgs() []string {
	return []string{"get", "-no-color"}
}

// terraformActionArgs is the terraform apply argument list, or a plan with a detailed exit code for a
// dry run, shared by the host runner and the container plan. The detailed exit code makes plan return
// 2 on pending changes, so a dry run tells a clean state from a drifted one instead of always
// exiting 0. A plan given PlanOut saves its plan file there, and an apply given PlanFile applies that
// saved plan, which needs no approval flag because the plan is the approval.
func terraformActionArgs(spec Spec) []string {
	if spec.DryRun {
		args := []string{"plan", "-input=false", "-no-color", "-detailed-exitcode"}
		if spec.PlanOut != "" {
			args = append(args, "-out="+spec.PlanOut)
		}
		return args
	}
	if spec.PlanFile != "" {
		return []string{"apply", "-input=false", "-no-color", spec.PlanFile}
	}
	return []string{"apply", "-auto-approve", "-input=false", "-no-color"}
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

// WorkDir returns the directory a Terraform or OpenTofu run works in: sub inside the checkout at
// base, or sub itself for a run with no checkout. It refuses a sub that leaves the checkout, by its
// path or through a link, as the runner does before it starts the tool.
func WorkDir(base, sub string) (string, error) {
	return toolWorkDir(base, sub)
}

// toolWorkDir resolves a tool's working directory from a base checkout and a subdirectory, rejecting
// one that escapes the base so a run cannot reach outside its project. With no base the subdirectory
// is used as given, relative to the process working directory, which is the command-line case where
// the operator names the directory themselves.
//
// Containment is checked against the resolved paths, not the spelling. The lexical check alone was
// satisfied by a subdirectory whose name is a symlink pointing out of the checkout: the string sits
// under the base, the directory does not. A project's own repository is enough to place one, so
// whoever could commit to a repository a template runs could aim terraform at any directory the
// server can read, and the container path builds its mount from this same value, so the same link
// mounted it into the container past the blocklist.
func toolWorkDir(base, sub string) (string, error) {
	if base == "" {
		return sub, nil
	}
	joined := filepath.Join(base, sub)
	rel, err := filepath.Rel(base, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrBadWorkDir
	}
	if err := checkResolvedUnder(base, joined); err != nil {
		return "", err
	}
	return joined, nil
}

// checkResolvedUnder reports whether target, with every symlink along it followed, still sits under
// base. A target that does not exist yet is judged by its nearest existing ancestor, because a run
// may legitimately create the directory it works in, and the part of the path that does exist is
// where a link can be hiding.
func checkResolvedUnder(base, target string) error {
	realBase, err := filepath.EvalSymlinks(base)
	if errors.Is(err, fs.ErrNotExist) {
		// A base that is not on this filesystem has no links to follow, so the lexical check is all
		// there is to check. That is the container plan built before a checkout exists, and the
		// command line naming a directory that will be created.
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadWorkDir, err)
	}
	probe := target
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			rel, relErr := filepath.Rel(realBase, resolved)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return ErrBadWorkDir
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", ErrBadWorkDir, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// Walked to the filesystem root without finding anything that exists, which cannot be
			// under the checkout.
			return ErrBadWorkDir
		}
		probe = parent
	}
}
