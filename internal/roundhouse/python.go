package roundhouse

import (
	"context"
	"io"
	"os/exec"
)

// pythonRunner runs a Python script as a child process. The script text comes from the Spec's
// Command, written to a temporary file and run with python3, so multi-line scripts and a dry-run
// syntax check both work. A dry run runs py_compile, which checks syntax without executing the
// script. Extra vars reach the script as SWITCHTENDER_VARS, a JSON object, and credentials arrive in
// Env; the working directory is the project checkout so the script can read project files.
type pythonRunner struct {
	// binary is the python executable name or path.
	binary string
	// baseEnv is the environment inherited by every execution.
	baseEnv []string
}

// newPythonRunner returns a pythonRunner that resolves python3 from PATH and inherits baseEnv.
func newPythonRunner(baseEnv []string) *pythonRunner {
	return &pythonRunner{binary: "python3", baseEnv: baseEnv}
}

// Run writes the script to a temp file and runs it with python3, streaming combined output to out. A
// dry run runs py_compile so syntax is checked without executing anything.
func (p *pythonRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Command == "" {
		return Result{ExitCode: -1}, ErrNoCommand
	}
	path, cleanup, err := writeScriptFile("switchtender-py-*.py", spec.Command)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, p.binary, pythonArgs(path, spec.DryRun)...)
	cmd.Dir = spec.Dir
	cmd.Env = varsEnv(p.baseEnv, spec)
	return runProcess(ctx, cmd, out)
}

// pythonArgs builds the python argument list for a script at path, shared by the host runner and the
// container plan. A dry run runs py_compile so syntax is checked without executing the script.
func pythonArgs(path string, dryRun bool) []string {
	if dryRun {
		return []string{"-m", "py_compile", path}
	}
	return []string{path}
}
