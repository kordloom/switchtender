package roundhouse

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// pwshRunner runs a PowerShell script as a child process. The script text comes from the Spec's
// Command, written to a temporary file and run with pwsh, so multi-line scripts and a dry-run
// parse check both work. Extra vars reach the script as RAILWARDEN_VARS, a JSON object, and
// credentials arrive in Env; the working directory is the project checkout so the script can read
// project files.
type pwshRunner struct {
	// binary is the PowerShell executable name or path.
	binary string
	// baseEnv is the environment inherited by every execution.
	baseEnv []string
}

// newPwshRunner returns a pwshRunner that resolves pwsh from PATH and inherits baseEnv.
func newPwshRunner(baseEnv []string) *pwshRunner {
	return &pwshRunner{binary: "pwsh", baseEnv: baseEnv}
}

// Run writes the script to a temp file and runs it with pwsh, streaming combined output to out. A
// dry run parses the script into a script block without invoking it, so syntax is checked without
// executing anything.
func (p *pwshRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Command == "" {
		return Result{ExitCode: -1}, ErrNoCommand
	}
	f, err := os.CreateTemp("", "railwarden-ps-*.ps1")
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := f.WriteString(spec.Command); err != nil {
		_ = f.Close()
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if err := f.Close(); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	args := []string{"-NoProfile", "-NonInteractive", "-File", path}
	if spec.DryRun {
		parse := fmt.Sprintf("[void][scriptblock]::Create((Get-Content -Raw '%s'))", path)
		args = []string{"-NoProfile", "-NonInteractive", "-Command", parse}
	}
	cmd := exec.CommandContext(ctx, p.binary, args...)
	cmd.Dir = spec.Dir
	cmd.Env = varsEnv(p.baseEnv, spec)
	return runProcess(ctx, cmd, out)
}
