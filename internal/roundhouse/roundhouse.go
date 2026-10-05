// Package roundhouse executes engines (playbooks) as subprocesses.
// The roundhouse is where engines are run; in SwitchTender terms it is the execution environment.
package roundhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
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
	// RunFilesSize caps the in-memory filesystem a containerized run's private directory is mounted
	// as, in the tmpfs size form, for example "64m". It counts against Memory as it fills.
	RunFilesSize string
}

// DefaultRunFilesSize is the cap on a containerized run's in-memory secrets mount when none is set.
// The mount holds the state cloud tools keep about the run's credentials, which is small.
const DefaultRunFilesSize = "64m"

// DefaultContainerLimits returns bounded defaults that keep normal runs working while stopping a
// single container from exhausting host memory, CPU, or process tables.
func DefaultContainerLimits() ContainerLimits {
	return ContainerLimits{Memory: "2g", CPUs: "2", PidsLimit: 2048, Network: "bridge",
		RunFilesSize: DefaultRunFilesSize}
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

// VaultPassword is one Ansible Vault password file, optionally labeled for --vault-id.
type VaultPassword struct {
	// Label is the vault id, empty for the classic unlabeled password.
	Label string
	// Path is the password file on disk.
	Path string
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
	// ModulesInstalled says a Terraform or OpenTofu working directory already holds exactly the
	// modules the run must use, put there and checked against what the approval gate read. Init
	// then installs no module, so it cannot resolve a version again and fetch something newer.
	ModulesInstalled bool
	// PlanOut, on a Terraform or OpenTofu dry run, is where the plan saves its plan file. The runner
	// returns the saved plan and its JSON rendering in the Result. Empty plans without saving. It is a
	// path in the run's private directory, because a plan file holds sensitive values.
	PlanOut string
	// PlanFile, on a Terraform or OpenTofu apply, is a saved plan file the apply carries out instead of
	// planning again, so the tool applies exactly that plan and refuses one that has gone stale. Empty
	// applies the configuration directly.
	PlanFile string
	// AddSecrets, when set, receives secret values a runner learns while it runs, such as the values a
	// saved plan marks sensitive, before the runner writes output that could contain them.
	AddSecrets func([]string)
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
	// Tags, when set, runs only plays and tasks carrying one of these tags, passed as a single
	// comma-separated ansible-playbook --tags argument.
	Tags []string
	// SkipTags, when set, skips plays and tasks carrying one of these tags, passed as a single
	// comma-separated ansible-playbook --skip-tags argument.
	SkipTags []string
	// Verbosity raises ansible-playbook logging from 0 (off) to 4 (-vvvv), for debugging a run
	// without changing the playbook.
	Verbosity int
	// Forks sets how many hosts ansible-playbook addresses in parallel, passed as --forks. Zero
	// leaves Ansible's own default.
	Forks int
	// DiffMode shows the before-and-after of every file and template change, passed as --diff.
	DiffMode bool
	// FactCacheDir, when set, points Ansible's jsonfile fact cache plugin at this directory, which
	// holds the cached facts written for the run and receives the facts it gathers. A container run
	// mounts it writable.
	FactCacheDir string
	// CredentialFiles are host paths a credential injection wrote, whose locations reach the tool
	// through environment variables. A container has to mount them or the variable names a path that
	// is not there, so they are tracked here rather than only in the environment.
	CredentialFiles []string
	// PrivateKeyPath, when set, is passed as ansible-playbook --private-key.
	PrivateKeyPath string
	// VaultPasswords are the Ansible Vault passwords for the run. An unlabeled one is passed as
	// --vault-password-file; a labeled one as --vault-id label@file, so several vaults on one run
	// each unlock the secrets encrypted for their label.
	VaultPasswords []VaultPassword
	// Image, when set, names a container image to run the playbook inside instead of on the host.
	Image string
	// RegistryUsername is the login for pulling a private Image. Empty needs no login.
	RegistryUsername string
	// RegistryPassword is the password for RegistryUsername, fed to docker login on stdin.
	RegistryPassword string
	// RunDir is the run's private directory, mode 0700 and removed when the run ends, swept if the
	// executor dies. The extra vars file, an inline script, a container run's environment file, and a
	// registry login are written inside it rather than into the shared temporary directory, and a
	// containerized run mounts an in-memory filesystem at the same path inside the container, so what
	// the tool writes there stays in the container's memory. Empty keeps the temporary directory.
	RunDir string
	// RunFilesRoot is the private root every run directory lives under. A containerized run may bind
	// mount a path inside it even where the root sits under a directory the mount guard otherwise
	// refuses, such as a systemd runtime directory under /run, because the root holds nothing but
	// what SwitchTender staged for runs.
	RunFilesRoot string
}

// Result is the outcome of a completed execution.
type Result struct {
	// ExitCode is the process exit code. Zero means success.
	ExitCode int
	// Drift reports that a dry run found pending changes, so the target has drifted from its desired
	// state. It is set by a tool with a no-change check that distinguishes a clean plan from a
	// changed one, such as a Terraform plan with a detailed exit code.
	Drift bool
	// ImageDigest is the digest of the image a container run pulled and ran, empty for a run on the
	// host or when the runtime could not say.
	ImageDigest string
	// PlanFile is the plan file a Terraform or OpenTofu dry run saved to Spec.PlanOut, empty when it
	// saved none. It holds sensitive values and is never written to the run's output.
	PlanFile []byte
	// PlanJSON is that plan as show -json renders it, for measuring what it changes, empty when it
	// could not be rendered. It holds sensitive values in the clear and is never written to the run's
	// output.
	PlanJSON []byte
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

// ModuleFetcher downloads the modules a Terraform or OpenTofu working directory calls, where and
// how a run of that directory would run the tool, and does nothing else. The approval gate asks it
// for a private copy of a configuration before it reads the registry and remote modules a plan
// would execute.
type ModuleFetcher interface {
	// FetchModules runs the tool's get for spec, streaming combined output to out. A get that runs
	// to completion with a non-zero exit returns a Result with that code and a nil error.
	FetchModules(ctx context.Context, spec Spec, out io.Writer) (Result, error)
}

// ModuleFetcherFunc adapts a function to the ModuleFetcher interface.
type ModuleFetcherFunc func(ctx context.Context, spec Spec, out io.Writer) (Result, error)

// FetchModules calls f.
func (f ModuleFetcherFunc) FetchModules(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	return f(ctx, spec, out)
}

// pluginCache materializes the embedded callback plugin to a temp directory and keeps that copy
// honest.
type pluginCache struct {
	// once guards one time creation of the directory.
	once sync.Once
	// mu serializes the integrity check, so two runs starting at the same moment cannot both be
	// halfway through rewriting the file.
	mu sync.Mutex
	// dir is the temp directory holding the materialized callback plugin.
	dir string
	// err records a failure to materialize the callback plugin.
	err error
}

// ensure returns the directory holding the callback plugin, creating it on first use and restoring
// the plugin whenever what is on disk is not what is embedded here.
//
// The path is handed to Ansible as ANSIBLE_CALLBACK_PLUGINS, so Ansible imports that file on every
// Ansible run. A host run executes as the server's own user, the same user that owns this directory,
// so a playbook, a bash script, or a python step can write to it. Materializing once and trusting the
// copy forever meant one run could leave code there that every later run on the host then imported,
// silently and for good. The file mode was never the obstacle, because the uid was the same.
//
// Checking before each use does not isolate one run from another, which needs a separate user or a
// container. It removes the persistent implant, which is the version of this that survives restarts
// and shows up in no log.
func (p *pluginCache) ensure() (string, error) {
	p.once.Do(func() {
		dir, err := os.MkdirTemp("", "switchtender-plugin-")
		if err != nil {
			p.err = err
			return
		}
		p.dir = dir
	})
	if p.err != nil {
		return "", p.err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	path := filepath.Join(p.dir, pluginName+".py")
	current, err := os.ReadFile(path)
	switch {
	case err == nil && string(current) == callbackPlugin:
		return p.dir, nil
	case err == nil:
		// Something rewrote it. That is either tampering or a broken deploy, and both are worth a line
		// somebody can find later; the run continues on the restored copy rather than failing, because
		// refusing to run would hand a denial of service to whoever wrote the file.
		p.log("roundhouse: the callback plugin on disk did not match the embedded copy and was " +
			"restored; a process running as this user modified " + path)
	}
	if err := os.WriteFile(path, []byte(callbackPlugin), 0o600); err != nil {
		return "", err
	}
	return p.dir, nil
}

// log writes a line about the plugin copy. It goes to standard error rather than through a logger
// because the cache is shared by both runners and holds no logger of its own, and this is a line an
// operator greps for after the fact rather than something a caller handles.
func (p *pluginCache) log(msg string) {
	fmt.Fprintln(os.Stderr, msg)
}

// ansibleRunner runs ansible-playbook as a child process.
type ansibleRunner struct {
	// binary is the ansible-playbook executable name or path.
	binary string
	// locator decides where the Ansible commands are started from: a configured directory, the
	// managed runtime, or PATH. Nil is PATH.
	locator *ansibleruntime.Locator
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

// WithAnsibleLocator starts the host's Ansible commands from where l says, asked again for every
// command, so a managed runtime installed or removed while the process runs takes effect on the
// next run. The commands are always started as separate programs.
func WithAnsibleLocator(l *ansibleruntime.Locator) Option {
	return func(a *ansibleRunner) { a.locator = l }
}

// AnsibleCommands reports where the host's Ansible commands come from now. A run calls it once and
// binds the answer to its context with WithAnsibleCommands, so its inventory reads, its play, and
// its evidence all name the same Ansible.
func (a *ansibleRunner) AnsibleCommands() ansibleruntime.Commands {
	return a.locator.Locate()
}

// ansibleKey types the context value carrying a run's located Ansible commands.
type ansibleKey struct{}

// WithAnsibleCommands binds cmds to ctx, so every Ansible command the host runner starts for the
// work ctx carries comes from cmds rather than from a fresh look. A run locates its Ansible once,
// binds it, and records it in its evidence, and an install or remove that lands while it runs
// changes nothing about it.
func WithAnsibleCommands(ctx context.Context, cmds ansibleruntime.Commands) context.Context {
	return context.WithValue(ctx, ansibleKey{}, cmds)
}

// AnsibleCommandsFrom returns the Ansible commands bound to ctx, and false when none are.
func AnsibleCommandsFrom(ctx context.Context) (ansibleruntime.Commands, bool) {
	cmds, ok := ctx.Value(ansibleKey{}).(ansibleruntime.Commands)
	return cmds, ok
}

// commandsFor returns the Ansible commands for the work ctx carries: those bound to it, or located
// now when none are.
func (a *ansibleRunner) commandsFor(ctx context.Context) ansibleruntime.Commands {
	if cmds, ok := AnsibleCommandsFrom(ctx); ok {
		return cmds
	}
	return a.locator.Locate()
}

// command returns what to start the Ansible command name as for the work ctx carries, its path in
// the located directory or the bare name for PATH to find, and the function that releases the hold
// that keeps a managed runtime from being removed while the command runs. A located directory that
// lacks the command is still where it is started from, so a misconfigured directory fails the run
// rather than quietly running another Ansible nobody chose, and a managed runtime that cannot be
// used fails it with the reason.
func (a *ansibleRunner) command(ctx context.Context, name string) (string, func(), error) {
	cmds := a.commandsFor(ctx)
	path, err := cmds.Command(name)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrAnsibleMissing, err)
	}
	release, err := cmds.Hold()
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	return path, release, nil
}

// NewAnsibleRunner returns a Runner that executes each Spec by its Tool: ansible-playbook for
// Ansible and bash for bash. By default it resolves the tool binaries from PATH and inherits the
// process environment. Container execution is off, so an image-bound Spec fails clearly.
func NewAnsibleRunner(opts ...Option) Runner {
	return newToolRouter(false, "docker", "missing", false, DefaultContainerLimits(), opts...)
}

// newAnsibleRunner builds the concrete host runner so callers that need its HostLister and
// InventoryDumper methods, such as the tool router, can hold the concrete type.
func newAnsibleRunner(opts ...Option) *ansibleRunner {
	a := &ansibleRunner{
		binary: defaultPlaybookBinary,
		// The run inherits the host's environment but not SwitchTender's own configuration. The
		// master encryption key, its salt, and every provider secret are read from there, and a run
		// is exactly what they protect.
		baseEnv: util.RunEnviron(),
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
// when a Spec names an image and allowContainer is set, inside that image; bash on the host. The
// pull policy sets the image --pull behavior, requireDigest rejects an image not pinned to a digest,
// and every container run is bounded by limits.
func NewSelectiveRunner(allowContainer bool, runtime, pullPolicy string, requireDigest bool,
	limits ContainerLimits, opts ...Option) Runner {
	return newToolRouter(allowContainer, runtime, pullPolicy, requireDigest, limits, opts...)
}

// newToolRouter builds the tool router shared by the Ansible and selective constructors.
func newToolRouter(allowContainer bool, runtime, pullPolicy string, requireDigest bool,
	limits ContainerLimits, opts ...Option) *toolRouter {
	host := newAnsibleRunner(opts...)
	return &toolRouter{
		ansibleRunner:  host,
		bash:           newBashRunner(host.baseEnv),
		terraform:      newTerraformRunner(host.baseEnv),
		opentofu:       &terraformRunner{binary: "tofu", baseEnv: host.baseEnv},
		python:         newPythonRunner(host.baseEnv),
		powershell:     newPwshRunner(host.baseEnv),
		golang:         newGoRunner(host.baseEnv),
		container:      newContainerRunner(runtime, pullPolicy, requireDigest, host.baseEnv, &host.plugin, limits),
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
	if run.IsBuiltinTool(tool) {
		panic("roundhouse: cannot override the built-in tool " + tool)
	}
	if _, exists := extraRunners[run.NormalizeTool(tool)]; exists {
		panic("roundhouse: duplicate runner for " + tool)
	}
	extraRunners[run.NormalizeTool(tool)] = r
}

// Run dispatches spec to the runner for its Tool, defaulting an empty Tool to Ansible. Any built-in
// tool that pins an image runs inside that image when container execution is allowed, otherwise it
// runs on the host. A Tool that matches no built-in falls through to a runner added with
// RegisterRunner.
func (t *toolRouter) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	tool := run.NormalizeTool(spec.Tool)
	if spec.Image != "" && isBuiltinTool(tool) {
		if !t.allowContainer {
			return Result{ExitCode: -1}, ErrContainerDisabled
		}
		return t.container.Run(ctx, spec, out)
	}
	switch tool {
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
		return t.ansibleRunner.Run(ctx, spec, out)
	default:
		if r, ok := extraRunners[tool]; ok {
			return r.Run(ctx, spec, out)
		}
		return Result{ExitCode: -1}, fmt.Errorf("%w: %s", ErrUnknownTool, spec.Tool)
	}
}

// FetchModules runs the get of spec's Terraform or OpenTofu working directory the way Run would run
// the tool there: with the host's binary and environment, or inside spec's image with the image's
// binary, the same mounts, limits, and network. It downloads the modules the configuration calls
// and nothing else, so it installs no provider and runs no program the configuration names, and it
// is handed no variables, since a get needs none.
func (t *toolRouter) FetchModules(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	tool := run.NormalizeTool(spec.Tool)
	if tool != run.ToolTerraform && tool != run.ToolOpenTofu {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %s calls no modules to fetch", ErrUnknownTool,
			spec.Tool)
	}
	spec.ExtraVars = nil
	if spec.Image != "" {
		if !t.allowContainer {
			return Result{ExitCode: -1}, ErrContainerDisabled
		}
		return t.container.runBuilt(ctx, spec, out, buildModulesPlan)
	}
	if tool == run.ToolOpenTofu {
		return t.opentofu.fetchModules(ctx, spec, out)
	}
	return t.terraform.fetchModules(ctx, spec, out)
}

// pluginName is the callback plugin name, matching the embedded file and its CALLBACK_NAME.
const pluginName = "switchtender"

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
	env = append(env, factCacheEnv(spec.FactCacheDir)...)

	varsCleanup, err := materializeExtraVars(&spec)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	defer varsCleanup()
	pargs, err := playbookArgs(spec)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	bin := a.binary
	if bin == defaultPlaybookBinary {
		path, release, err := a.command(ctx, bin)
		if err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
		}
		defer release()
		bin = path
	}
	cmd := exec.CommandContext(ctx, bin, pargs...)
	cmd.Dir = spec.Dir
	cmd.Env = env
	return runProcess(ctx, cmd, out)
}

// materializeExtraVars moves a Spec's extra vars off the command line and into a private file,
// returning the cleanup that removes it.
//
// Every credential-derived value already takes this route, deliberately, so the password stays off
// argv. The run's own extra vars did not: they were marshaled to JSON and handed to ansible-playbook
// as --extra-vars, where ps auxww shows them to any local account on the executor for the life of the
// run. A template survey collects them, there is no password field type so a secret is collected as
// ordinary text, and for a container run the same argv becomes the docker command line and is kept in
// the container's Config.Cmd, readable by docker inspect after the run has finished. Ansible was the
// only tool that did this; the script tools pass their vars in the environment.
//
// The spec is updated in place so the caller's mount list picks the file up, and the inline copy is
// cleared so the two cannot both be sent.
func materializeExtraVars(spec *Spec) (func(), error) {
	noCleanup := func() {}
	if len(spec.ExtraVars) == 0 {
		return noCleanup, nil
	}
	data, err := json.Marshal(spec.ExtraVars)
	if err != nil {
		return noCleanup, fmt.Errorf("marshal extra vars: %w", err)
	}
	f, err := os.CreateTemp(spec.RunDir, "switchtender-vars-*.json")
	if err != nil {
		return noCleanup, fmt.Errorf("create extra vars file: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return noCleanup, fmt.Errorf("secure extra vars file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return noCleanup, fmt.Errorf("write extra vars file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return noCleanup, fmt.Errorf("close extra vars file: %w", err)
	}
	spec.ExtraVarsFiles = append(spec.ExtraVarsFiles, f.Name())
	spec.ExtraVars = nil
	return cleanup, nil
}

// runProcess supervises cmd, streaming its combined stdout and stderr to out, and maps the outcome
// to a Result. A clean exit is code zero; a non-zero exit returns that code with a nil error; a
// canceled context is reported as cancellation; any other failure wraps ErrLaunch. It is shared by
// every tool runner so they supervise child processes identically.
func runProcess(ctx context.Context, cmd *exec.Cmd, out io.Writer) (Result, error) {
	cmd.Stdout = out
	cmd.Stderr = out
	configureProcessGroup(cmd)
	err := runSupervised(cmd, nil)
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

// writeScriptFile writes content to a private temp file matching pattern in the temporary directory
// and returns its path and a cleanup that removes it.
func writeScriptFile(pattern, content string) (string, func(), error) {
	return writeScriptFileIn("", pattern, content)
}

// writeScriptFileIn writes content to a private temp file matching pattern in dir, the temporary
// directory when dir is empty, and returns its path and a cleanup that removes it. The script tools
// share it so a run's inline source reaches a host process and a container mount the same way, and
// pass the run's private directory so a script that carries a secret is swept with the run's other
// secrets when its executor dies.
func writeScriptFileIn(dir, pattern, content string) (string, func(), error) {
	noop := func() {}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", noop, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	path := f.Name()
	remove := func() { _ = os.Remove(path) }
	// A failure below removes the file here rather than returning a cleanup for the caller to
	// remember. Every caller returns on the error and drops the cleanup, so a failed write or
	// close left the script on disk for good, and that script is the run's command verbatim with
	// whatever credentials the operator inlined into it.
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		remove()
		return "", noop, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", noop, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	return path, remove, nil
}

// varsEnv layers a Spec's credential env and, when it has extra vars, a JSON SWITCHTENDER_VARS of them
// over the base environment, so bash and python runs read survey answers and template vars from one
// place, the same way.
func varsEnv(baseEnv []string, spec Spec) []string {
	env := append(append([]string{}, baseEnv...), spec.Env...)
	return append(env, varsExtra(spec)...)
}

// varsExtra returns the SWITCHTENDER_VARS entry carrying a Spec's extra vars as JSON, and one
// SWITCHTENDER_VAR_<name> entry per scalar var, or nil when there are none. It is shared by the host
// script runners and the container plan.
//
// The single entries exist so a script can read one answer with plain shell. With only the JSON, a
// script needed a parser to reach a survey answer, and a job imported from Jenkins or Rundeck, which
// read each parameter as a variable of its own, ran with every one of them empty. The prefix keeps
// an answer from ever landing on PATH, LD_PRELOAD, or any other variable the process runs by.
func varsExtra(spec Spec) []string {
	if len(spec.ExtraVars) == 0 {
		return nil
	}
	b, err := json.Marshal(spec.ExtraVars)
	if err != nil {
		return nil
	}
	out := []string{"SWITCHTENDER_VARS=" + string(b)}
	for _, name := range slices.Sorted(maps.Keys(spec.ExtraVars)) {
		if !envVarName.MatchString(name) {
			continue
		}
		if value, ok := envScalar(spec.ExtraVars[name]); ok {
			out = append(out, run.VarEnvPrefix+name+"="+value)
		}
	}
	return out
}

// envVarName matches a name a shell variable can hold.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envScalar renders a scalar extra var as the text its environment entry carries, reporting false
// for a value that is not a scalar or that an environment entry cannot hold.
func envScalar(v any) (string, bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case bool:
		s = strconv.FormatBool(t)
	case json.Number:
		s = t.String()
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		s = strconv.Itoa(t)
	case int64:
		s = strconv.FormatInt(t, 10)
	default:
		return "", false
	}
	if strings.IndexByte(s, 0) >= 0 {
		return "", false
	}
	return s, true
}

// callbackEnv returns the environment entries that enable the structured event callback and point
// it at the events sidecar file.
func callbackEnv(pluginDir, eventsPath string) []string {
	return []string{
		"ANSIBLE_CALLBACK_PLUGINS=" + pluginDir,
		"ANSIBLE_CALLBACKS_ENABLED=" + pluginName,
		"SWITCHTENDER_EVENTS_PATH=" + eventsPath,
	}
}

// factCacheEnv returns the environment entries that point Ansible's fact cache at dir, or none when
// dir is empty. They are the settings AWX uses: the jsonfile plugin reading and writing one file
// per host in the run's own directory. The plugin's own timeout is off because the cache was
// filtered by the template's timeout when it was written, and a long run must not expire facts
// partway through.
func factCacheEnv(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{
		"ANSIBLE_CACHE_PLUGIN=jsonfile",
		"ANSIBLE_CACHE_PLUGIN_CONNECTION=" + dir,
		"ANSIBLE_CACHE_PLUGIN_TIMEOUT=0",
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
	if len(spec.Tags) > 0 {
		args = append(args, "--tags", strings.Join(spec.Tags, ","))
	}
	if len(spec.SkipTags) > 0 {
		args = append(args, "--skip-tags", strings.Join(spec.SkipTags, ","))
	}
	if spec.Forks > 0 {
		args = append(args, "--forks", strconv.Itoa(spec.Forks))
	}
	if spec.DiffMode {
		args = append(args, "--diff")
	}
	// Verbosity is one -v per level, capped at four, which is ansible-playbook's most verbose.
	if v := spec.Verbosity; v > 0 {
		if v > 4 {
			v = 4
		}
		args = append(args, "-"+strings.Repeat("v", v))
	}
	if spec.DryRun {
		args = append(args, "--check")
	}
	for _, file := range spec.ExtraVarsFiles {
		args = append(args, "--extra-vars", "@"+file)
	}
	if spec.PrivateKeyPath != "" {
		args = append(args, "--private-key", spec.PrivateKeyPath)
	}
	for _, vp := range spec.VaultPasswords {
		if vp.Label == "" {
			args = append(args, "--vault-password-file", vp.Path)
			continue
		}
		args = append(args, "--vault-id", vp.Label+"@"+vp.Path)
	}
	// The playbook is separated from the options it follows. Without this a playbook whose name
	// begins with a dash was read by ansible-playbook as another option rather than as the file to
	// run, so a stored template could turn its own name into a flag.
	return append(args, "--", spec.Playbook), nil
}
