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
	args := make([]string, 0, 3)
	if spec.DryRun {
		args = append(args, "-n")
	}
	args = append(args, "-c", spec.Command)
	cmd := exec.CommandContext(ctx, b.binary, args...)
	cmd.Dir = spec.Dir
	cmd.Env = varsEnv(b.baseEnv, spec)
	return runProcess(ctx, cmd, out)
}
