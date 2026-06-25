// Package roundhouse executes engines (playbooks) as subprocesses.
// The roundhouse is where engines are run; in Yardmaster terms it is the execution environment.
package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Spec describes a single playbook execution.
type Spec struct {
	// Playbook is the path to the Ansible playbook file.
	Playbook string
	// Inventory is the path to the Ansible inventory. When empty, no -i flag is passed.
	Inventory string
	// ExtraVars are passed to ansible-playbook as repeated --extra-vars key=value flags.
	ExtraVars map[string]string
	// Env holds additional environment entries (KEY=VALUE) layered over the base environment.
	Env []string
	// Dir is the working directory for the process. When empty, the current directory is used.
	Dir string
}

// Result is the outcome of a completed execution.
type Result struct {
	// ExitCode is the process exit code. Zero means success.
	ExitCode int
}

// Runner executes a Spec, streaming combined output to out, and reports the Result.
// A non-nil error means the process could not be launched or supervised; a playbook that
// runs to completion with a non-zero exit returns a Result with that code and a nil error.
type Runner interface {
	Run(ctx context.Context, spec Spec, out io.Writer) (Result, error)
}

// RunnerFunc adapts a function to the Runner interface.
type RunnerFunc func(ctx context.Context, spec Spec, out io.Writer) (Result, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	return f(ctx, spec, out)
}

// ansibleRunner runs ansible-playbook as a child process.
type ansibleRunner struct {
	// binary is the ansible-playbook executable name or path.
	binary string
	// baseEnv is the environment inherited by every execution.
	baseEnv []string
}

// Option configures an ansibleRunner.
type Option func(*ansibleRunner)

// WithBinary overrides the ansible-playbook executable name or path.
func WithBinary(binary string) Option {
	return func(a *ansibleRunner) { a.binary = binary }
}

// WithBaseEnv overrides the environment inherited by every execution.
func WithBaseEnv(env []string) Option {
	return func(a *ansibleRunner) { a.baseEnv = env }
}

// NewAnsibleRunner returns a Runner that shells out to ansible-playbook.
// By default it resolves ansible-playbook from PATH and inherits the process environment.
func NewAnsibleRunner(opts ...Option) Runner {
	a := &ansibleRunner{
		binary:  "ansible-playbook",
		baseEnv: os.Environ(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run executes the playbook described by spec and streams combined stdout and stderr to out.
func (a *ansibleRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Playbook == "" {
		return Result{ExitCode: -1}, ErrNoPlaybook
	}

	cmd := exec.CommandContext(ctx, a.binary, a.args(spec)...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Dir = spec.Dir
	cmd.Env = append(append([]string{}, a.baseEnv...), spec.Env...)

	err := cmd.Run()
	if err == nil {
		return Result{ExitCode: 0}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return Result{ExitCode: exitErr.ExitCode()}, nil
	}
	return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
}

// args builds the ansible-playbook argument list for spec.
func (a *ansibleRunner) args(spec Spec) []string {
	args := make([]string, 0, 4+2*len(spec.ExtraVars))
	if spec.Inventory != "" {
		args = append(args, "-i", spec.Inventory)
	}
	for k, v := range spec.ExtraVars {
		args = append(args, "--extra-vars", k+"="+v)
	}
	return append(args, spec.Playbook)
}
