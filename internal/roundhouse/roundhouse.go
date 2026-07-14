// Package roundhouse executes engines (playbooks) as subprocesses.
// The roundhouse is where engines are run; in Railwarden terms it is the execution environment.
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

	"github.com/dcadolph/railwarden/internal/run"
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
	// Tool selects the execution engine: ansible, bash, terraform, opentofu, python, powershell, or go. Empty means ansible.
	Tool string
	// Command carries the tool's primary input for non-Ansible tools: the script for bash and python,
	// the source for go, the working directory for terraform.
	Command string
	// DryRun runs the tool in its no-change mode: ansible --check, a syntax check for bash.
	DryRun bool
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
		dir, err := os.MkdirTemp("", "railwarden-plugin-")
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

// NewAnsibleRunner returns a Runner that executes each Spec by its Tool: ansible-playbook for
// Ansible and bash for bash. By default it resolves the tool binaries from PATH and inherits the
// process environment. Container execution is off, so an image-bound Spec fails clearly.
func NewAnsibleRunner(opts ...Option) Runner {
	return newToolRouter(false, DefaultContainerLimits(), opts...)
}

// newAnsibleRunner builds the concrete host runner so callers that need its HostLister and
// InventoryDumper methods, such as the tool router, can hold the concrete type.
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

// toolRouter routes each Spec to the runner for its Tool. Ansible runs on the host, or inside a
// container when the Spec names an image and container execution is allowed; bash runs on the host.
// It embeds the host Ansible runner so its HostLister and InventoryDumper methods are promoted,
// which the dispatcher relies on for split runs and dynamic inventory.
type toolRouter struct {
	// ansibleRunner is the host Ansible runner and the source of host listing and inventory dumping.
	*ansibleRunner
	// bash runs bash Specs on the host.
	bash *bashRunner
	// terraform runs terraform Specs on the host.
	terraform *terraformRunner
	// opentofu runs opentofu Specs on the host with the tofu binary.
	opentofu *terraformRunner
	// python runs python Specs on the host.
	python *pythonRunner
	// powershell runs powershell Specs on the host with pwsh.
	powershell *pwshRunner
	// golang runs go Specs on the host.
	golang *goRunner
	// container runs an image-bound Ansible Spec inside its image.
	container *containerRunner
	// allowContainer gates container execution; when false an image-bound Spec fails clearly.
	allowContainer bool
}

// NewSelectiveRunner returns a Runner that executes each Spec by its Tool: Ansible on the host or,
// when a Spec names an image and allowContainer is set, inside that image; bash on the host. Every
// container run is bounded by limits.
func NewSelectiveRunner(allowContainer bool, limits ContainerLimits, opts ...Option) Runner {
	return newToolRouter(allowContainer, limits, opts...)
}

// newToolRouter builds the tool router shared by the Ansible and selective constructors.
func newToolRouter(allowContainer bool, limits ContainerLimits, opts ...Option) *toolRouter {
	host := newAnsibleRunner(opts...)
	return &toolRouter{
		ansibleRunner:  host,
		bash:           newBashRunner(host.baseEnv),
		terraform:      newTerraformRunner(host.baseEnv),
		opentofu:       &terraformRunner{binary: "tofu", baseEnv: host.baseEnv},
		python:         newPythonRunner(host.baseEnv),
		powershell:     newPwshRunner(host.baseEnv),
		golang:         newGoRunner(host.baseEnv),
		container:      newContainerRunner(host.baseEnv, &host.plugin, limits),
		allowContainer: allowContainer,
	}
}

// extraRunners holds runners added by an extension, keyed by tool name. RegisterRunner adds a tool
// without editing the router, and Run dispatches to it when a Spec names it. Registration happens at
// startup, before runs execute, so reads need no lock, matching secretsource.
var extraRunners = map[string]Runner{}

// RegisterRunner records the Runner for a tool added by an extension. Pair it with run.RegisterTool
// so the tool passes validation. It panics on an empty or duplicate name, a nil runner, or an
// attempt to override a built-in, which is a programming error caught at startup.
func RegisterRunner(tool string, r Runner) {
	if tool == "" {
		panic("roundhouse: cannot register an empty tool name")
	}
	if r == nil {
		panic("roundhouse: nil runner for " + tool)
	}
	switch run.NormalizeTool(tool) {
	case run.ToolAnsible, run.ToolBash, run.ToolTerraform, run.ToolOpenTofu, run.ToolPython, run.ToolPowerShell, run.ToolGo:
		panic("roundhouse: cannot override the built-in tool " + tool)
	}
	if _, exists := extraRunners[run.NormalizeTool(tool)]; exists {
		panic("roundhouse: duplicate runner for " + tool)
	}
	extraRunners[run.NormalizeTool(tool)] = r
}

// Run dispatches spec to the runner for its Tool, defaulting an empty Tool to Ansible. A Tool that
// matches no built-in falls through to a runner added with RegisterRunner.
func (t *toolRouter) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	switch run.NormalizeTool(spec.Tool) {
	case run.ToolBash:
		return t.bash.Run(ctx, spec, out)
	case run.ToolTerraform:
		return t.terraform.Run(ctx, spec, out)
	case run.ToolOpenTofu:
		return t.opentofu.Run(ctx, spec, out)
	case run.ToolPython:
		return t.python.Run(ctx, spec, out)
	case run.ToolPowerShell:
		return t.powershell.Run(ctx, spec, out)
	case run.ToolGo:
		return t.golang.Run(ctx, spec, out)
	case run.ToolAnsible:
		if spec.Image == "" {
			return t.ansibleRunner.Run(ctx, spec, out)
		}
		if !t.allowContainer {
			return Result{ExitCode: -1}, ErrContainerDisabled
		}
		return t.container.Run(ctx, spec, out)
	default:
		if r, ok := extraRunners[run.NormalizeTool(spec.Tool)]; ok {
			return r.Run(ctx, spec, out)
		}
		return Result{ExitCode: -1}, fmt.Errorf("%w: %s", ErrUnknownTool, spec.Tool)
	}
}

// pluginName is the callback plugin name, matching the embedded file and its CALLBACK_NAME.
const pluginName = "railwarden"

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
	cmd.Dir = spec.Dir
	cmd.Env = env
	return runProcess(ctx, cmd, out)
}

// runProcess supervises cmd, streaming its combined stdout and stderr to out, and maps the outcome
// to a Result. A clean exit is code zero; a non-zero exit returns that code with a nil error; a
// canceled context is reported as cancellation; any other failure wraps ErrLaunch. It is shared by
// every tool runner so they supervise child processes identically.
func runProcess(ctx context.Context, cmd *exec.Cmd, out io.Writer) (Result, error) {
	cmd.Stdout = out
	cmd.Stderr = out
	configureProcessGroup(cmd)
	err := cmd.Run()
	if err == nil {
		return Result{ExitCode: 0}, nil
	}
	// A canceled context kills the process, which surfaces as an ExitError. Report the context error
	// so the caller treats it as cancellation rather than a tool failure.
	if ctx.Err() != nil {
		return Result{ExitCode: -1}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return Result{ExitCode: exitErr.ExitCode()}, nil
	}
	return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
}

// varsEnv layers a Spec's credential env and, when it has extra vars, a JSON RAILWARDEN_VARS of them
// over the base environment, so bash and python runs read survey answers and template vars from one
// place, the same way.
func varsEnv(baseEnv []string, spec Spec) []string {
	env := append(append([]string{}, baseEnv...), spec.Env...)
	if len(spec.ExtraVars) > 0 {
		if b, err := json.Marshal(spec.ExtraVars); err == nil {
			env = append(env, "RAILWARDEN_VARS="+string(b))
		}
	}
	return env
}

// callbackEnv returns the environment entries that enable the structured event callback and point
// it at the events sidecar file.
func callbackEnv(pluginDir, eventsPath string) []string {
	return []string{
		"ANSIBLE_CALLBACK_PLUGINS=" + pluginDir,
		"ANSIBLE_CALLBACKS_ENABLED=" + pluginName,
		"RAILWARDEN_EVENTS_PATH=" + eventsPath,
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
	if spec.DryRun {
		args = append(args, "--check")
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
