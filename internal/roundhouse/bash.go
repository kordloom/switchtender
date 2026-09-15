package roundhouse

import (
	"context"
	"io"
	"os/exec"
)

// bashRunner runs a shell script with bash as a child process. The script text comes from the
// Spec's Command, executed with bash -c, so it can invoke any tool on the host: kubectl, aws,
// terraform, make, and so on. Materialized credentials arrive through the Spec's Env.
type bashRunner struct {
	// binary is the bash executable name or path.
	binary string
	// baseEnv is the environment inherited by every execution.
	baseEnv []string
}

// newBashRunner returns a bashRunner that resolves bash from PATH and inherits baseEnv.
func newBashRunner(baseEnv []string) *bashRunner {
	return &bashRunner{binary: "bash", baseEnv: baseEnv}
}

// Run executes the script in spec.Command with bash, streaming combined output to out. A dry run
// passes -n so bash parses the script and reports syntax errors without executing it. Materialized
// credentials arrive in the environment, extra vars as a JSON SWITCHTENDER_VARS, and spec.Dir sets the
// working directory so a project's files are in reach.
func (b *bashRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Command == "" {
		return Result{ExitCode: -1}, ErrNoCommand
	}
	// The script goes to a 0600 temp file rather than onto argv, the same way python, go and
	// powershell already do it.
	//
	// bash -c "<the whole script>" puts every byte of the script in the process argument list, which
	// is world-readable on Linux: any other account on the executor can read a running job's script
	// out of ps, and a containerized run keeps it in the container's Config.Cmd, where docker inspect
	// finds it long after the run ended and the temp files are gone. An operator who writes
	// password=... inline is doing something the product warns against, but the other three script
	// tools do not punish it this way, and a difference like that between sibling tools is not a
	// decision anybody made.
	path, cleanup, err := writeScriptFile("switchtender-sh-*.sh", spec.Command)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, b.binary, bashArgs(path, spec.DryRun)...)
	cmd.Dir = spec.Dir
	cmd.Env = varsEnv(b.baseEnv, spec)
	return runProcess(ctx, cmd, out)
}

// bashArgs builds the bash argument list for a script at path, shared by the host runner and the
// container plan. A dry run adds -n so bash parses the script and reports syntax errors without
// executing it.
func bashArgs(path string, dryRun bool) []string {
	args := make([]string, 0, 2)
	if dryRun {
		args = append(args, "-n")
	}
	return append(args, path)
}
