// Package roundhouse executes engines (playbooks) as subprocesses.
// The roundhouse is where engines are run; in Yardmaster terms it is the execution environment.
package roundhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
)

// ContainerLimits caps the resources and network a containerized run may use, so a foot-gun or
// malicious project cannot exhaust the host. An empty string or non-positive value omits that one
// cap.
type ContainerLimits struct {
	// Memory is the docker --memory value, for example "2g".
	Memory string
	// CPUs is the docker --cpus value, for example "2".
	CPUs string
	// PidsLimit is the docker --pids-limit value, capping the container's process table.
	PidsLimit int
	// Network is the docker --network value, for example "bridge" or "none".
	Network string
}

// DefaultContainerLimits returns bounded defaults that keep normal runs working while stopping a
// single container from exhausting host memory, CPU, or process tables.
func DefaultContainerLimits() ContainerLimits {
	return ContainerLimits{Memory: "2g", CPUs: "2", PidsLimit: 2048, Network: "bridge"}
}

// args returns the docker run flags for the configured limits, omitting any that are unset.
func (l ContainerLimits) args() []string {
	var a []string
	if l.Memory != "" {
		a = append(a, "--memory", l.Memory)
	}
	if l.CPUs != "" {
		a = append(a, "--cpus", l.CPUs)
	}
	if l.PidsLimit > 0 {
		a = append(a, "--pids-limit", strconv.Itoa(l.PidsLimit))
	}
	if l.Network != "" {
		a = append(a, "--network", l.Network)
	}
	return a
}

// Spec describes a single playbook execution.
type Spec struct {
	// Playbook is the path to the Ansible playbook file.
	Playbook string
	// Inventory is the path to the Ansible inventory. When empty, no -i flag is passed.
	Inventory string
	// ExtraVars are passed to ansible-playbook as one JSON --extra-vars argument so values keep
	// their types.
	ExtraVars map[string]any
	// ExtraVarsFiles are passed to ansible-playbook as --extra-vars @file arguments. They carry
	// values that must stay off the command line, such as a become password.
	ExtraVarsFiles []string
	// Env holds additional environment entries (KEY=VALUE) layered over the base environment.
	Env []string
	// Dir is the working directory for the process. When empty, the current directory is used.
	Dir string
	// EventsPath, when set, enables the structured event callback and names the file it writes.
	EventsPath string
	// Limit restricts execution to a host pattern, passed as ansible-playbook --limit.
	Limit string
	// PrivateKeyPath, when set, is passed as ansible-playbook --private-key.
	PrivateKeyPath string
	// VaultPasswordFile, when set, is passed as ansible-playbook --vault-password-file.
	VaultPasswordFile string
	// Image, when set, names a container image to run the playbook inside instead of on the host.
	Image string
	// RegistryUsername is the login for pulling a private Image. Empty needs no login.
	RegistryUsername string
	// RegistryPassword is the password for RegistryUsername, fed to docker login on stdin.
	RegistryPassword string
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

// pluginCache materializes the embedded callback plugin to a temp directory once and reuses it.
type pluginCache struct {
	// once guards one time materialization of the callback plugin.
	once sync.Once
	// dir is the temp directory holding the materialized callback plugin.
	dir string
	// err records a failure to materialize the callback plugin.
	err error
}

// ensure materializes the embedded callback plugin to a temp directory once and returns it.
func (p *pluginCache) ensure() (string, error) {
	p.once.Do(func() {
		dir, err := os.MkdirTemp("", "yardmaster-plugin-")
		if err != nil {
			p.err = err
			return
		}
		if err := os.WriteFile(filepath.Join(dir, pluginName+".py"), []byte(callbackPlugin), 0o600); err != nil {
			p.err = err
			return
		}
		p.dir = dir
	})
	return p.dir, p.err
}

// ansibleRunner runs ansible-playbook as a child process.
type ansibleRunner struct {
	// binary is the ansible-playbook executable name or path.
	binary string
	// baseEnv is the environment inherited by every execution.
	baseEnv []string
	// plugin materializes the callback plugin on first use.
	plugin pluginCache
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
	return newAnsibleRunner(opts...)
}

// newAnsibleRunner builds the concrete host runner so callers that need its HostLister and
// InventoryDumper methods, such as the selective runner, can hold the concrete type.
func newAnsibleRunner(opts ...Option) *ansibleRunner {
	a := &ansibleRunner{
		binary:  "ansible-playbook",
		baseEnv: os.Environ(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// selectRunner routes each Spec to the host runner or a container runner by its Image field. It
// embeds the host runner so its HostLister and InventoryDumper methods are promoted, which the
// dispatcher relies on for split runs and dynamic inventory.
type selectRunner struct {
	// ansibleRunner is the host runner, used when a Spec has no image and for host listing.
	*ansibleRunner
	// container runs a Spec inside its image.
	container *containerRunner
	// allowContainer gates container execution; when false an image-bound Spec fails clearly.
	allowContainer bool
}

// NewSelectiveRunner returns a Runner that executes on the host by default and inside a container
// when a Spec names an image. Container execution is refused unless allowContainer is set, and every
// container run is bounded by limits.
func NewSelectiveRunner(allowContainer bool, limits ContainerLimits, opts ...Option) Runner {
	host := newAnsibleRunner(opts...)
	return &selectRunner{
		ansibleRunner:  host,
		container:      newContainerRunner(host.baseEnv, &host.plugin, limits),
		allowContainer: allowContainer,
	}
}

// Run executes on the host when the Spec has no image, otherwise inside the image when container
// execution is allowed.
func (s *selectRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Image == "" {
		return s.ansibleRunner.Run(ctx, spec, out)
	}
	if !s.allowContainer {
		return Result{ExitCode: -1}, ErrContainerDisabled
	}
	return s.container.Run(ctx, spec, out)
}

// pluginName is the callback plugin name, matching the embedded file and its CALLBACK_NAME.
const pluginName = "yardmaster"

// Run executes the playbook described by spec and streams combined stdout and stderr to out.
// When spec.EventsPath is set, the structured event callback is enabled and writes to that file.
func (a *ansibleRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Playbook == "" {
		return Result{ExitCode: -1}, ErrNoPlaybook
	}

	env := append(append([]string{}, a.baseEnv...), spec.Env...)
	if spec.EventsPath != "" {
		dir, err := a.plugin.ensure()
		if err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
		}
		env = append(env, callbackEnv(dir, spec.EventsPath)...)
	}

	pargs, err := playbookArgs(spec)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	cmd := exec.CommandContext(ctx, a.binary, pargs...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Dir = spec.Dir
	cmd.Env = env

	err = cmd.Run()
	if err == nil {
		return Result{ExitCode: 0}, nil
	}

	// A canceled context kills the process, which surfaces as an ExitError. Report the context error
	// so the caller treats it as cancellation rather than a playbook failure.
	if ctx.Err() != nil {
		return Result{ExitCode: -1}, ctx.Err()
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return Result{ExitCode: exitErr.ExitCode()}, nil
	}
	return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
}

// callbackEnv returns the environment entries that enable the structured event callback and point
// it at the events sidecar file.
func callbackEnv(pluginDir, eventsPath string) []string {
	return []string{
		"ANSIBLE_CALLBACK_PLUGINS=" + pluginDir,
		"ANSIBLE_CALLBACKS_ENABLED=" + pluginName,
		"YARDMASTER_EVENTS_PATH=" + eventsPath,
	}
}

// playbookArgs builds the ansible-playbook argument list for spec, shared by the host and container
// runners so both invoke ansible-playbook identically. It errors when the extra vars cannot be
// encoded, so a run fails loudly rather than silently executing without a variable that may gate a
// destructive task.
func playbookArgs(spec Spec) ([]string, error) {
	args := make([]string, 0, 8)
	if spec.Inventory != "" {
		args = append(args, "-i", spec.Inventory)
	}
	if spec.Limit != "" {
		args = append(args, "--limit", spec.Limit)
	}
	if len(spec.ExtraVars) > 0 {
		data, err := json.Marshal(spec.ExtraVars)
		if err != nil {
			return nil, fmt.Errorf("marshal extra vars: %w", err)
		}
		args = append(args, "--extra-vars", string(data))
	}
	for _, file := range spec.ExtraVarsFiles {
		args = append(args, "--extra-vars", "@"+file)
	}
	if spec.PrivateKeyPath != "" {
		args = append(args, "--private-key", spec.PrivateKeyPath)
	}
	if spec.VaultPasswordFile != "" {
		args = append(args, "--vault-password-file", spec.VaultPasswordFile)
	}
	return append(args, spec.Playbook), nil
}
