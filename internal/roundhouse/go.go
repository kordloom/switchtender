package roundhouse

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// goRunner runs a Go program as a child process. The source text comes from the Spec's Command,
// written to a temporary main.go and run with go run, so a full program builds and runs in one step.
// A dry run runs go vet, which compiles and statically checks the source without executing it. Extra
// vars reach the program as YARDMASTER_VARS, a JSON object, and credentials arrive in Env; the
// working directory is the project checkout so the program can read project files.
type goRunner struct {
	// binary is the go executable name or path.
	binary string
	// baseEnv is the environment inherited by every execution.
	baseEnv []string
}

// newGoRunner returns a goRunner that resolves go from PATH and inherits baseEnv.
func newGoRunner(baseEnv []string) *goRunner {
	return &goRunner{binary: "go", baseEnv: baseEnv}
}

// Run writes the source to a temp main.go and runs it with go run, streaming combined output to out.
// A dry run runs go vet so the source is compiled and checked without executing anything, and reports
// the result to out so a clean check does not leave an empty log.
func (g *goRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Command == "" {
		return Result{ExitCode: -1}, ErrNoCommand
	}
	f, err := os.CreateTemp("", "yardmaster-go-*.go")
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

	args := []string{"run", path}
	if spec.DryRun {
		args = []string{"vet", path}
		// go vet is silent when it finds nothing, so announce the check up front. That keeps the
		// dry-run log informative rather than empty when the source is clean.
		_, _ = io.WriteString(out, "dry run: go vet checks the source without executing it\n")
	}
	cmd := exec.CommandContext(ctx, g.binary, args...)
	cmd.Dir = spec.Dir
	cmd.Env = varsEnv(g.baseEnv, spec)
	res, err := runProcess(ctx, cmd, out)
	if spec.DryRun && err == nil && res.ExitCode == 0 {
		_, _ = io.WriteString(out, "go vet: no problems found\n")
	}
	return res, err
}
