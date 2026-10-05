package roundhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/imageref"
)

// containerKillAttempts and containerKillInterval bound how persistently a canceled run tries to
// remove its container, so one that the daemon creates just after the cancel is still caught.
const (
	containerKillAttempts = 5
	containerKillInterval = time.Second
	// containerStopGrace is how long a canceled container gets to shut down before it is removed by
	// force. It matches the grace a canceled tool gets on the host, and for the same reason: it is
	// what lets terraform release its state lock and ansible stop between tasks rather than dying
	// mid-write. Declared here rather than reused from the host path because that constant is built
	// only on unix, while a container runs wherever the runtime does.
	containerStopGrace = 10 * time.Second
	// containerRemoveWait bounds how long a canceled run waits for its container to actually be gone
	// before returning. It covers the stop grace plus a few removal attempts, so the ordinary case
	// completes and a wedged daemon delays rather than hangs.
	containerRemoveWait = containerStopGrace + containerKillAttempts*containerKillInterval + 5*time.Second
)

// containerRunner executes a tool inside a container image so each project can pin its own tool
// binaries, Python, and system dependencies independent of the host.
type containerRunner struct {
	// runtime is the container CLI, docker or podman.
	runtime string
	// pullPolicy is the container --pull policy: always, missing, or never.
	pullPolicy string
	// requireDigest rejects an image reference that is not pinned to an @sha256: digest.
	requireDigest bool
	// baseEnv is the environment the host CLI inherits when pulling images and logging in.
	baseEnv []string
	// plugin materializes the callback plugin on first use, shared into the container read-only.
	plugin *pluginCache
	// limits caps the memory, CPU, process count, and network of every container run.
	limits ContainerLimits
}

// newContainerRunner builds a container runner for the given container CLI (docker or podman),
// sharing the host runner's plugin cache and base environment, bounded by limits. The pull policy
// sets the image --pull behavior and requireDigest rejects an image not pinned to a digest. An empty
// runtime defaults to docker and an empty pull policy defaults to missing.
func newContainerRunner(runtime, pullPolicy string, requireDigest bool, baseEnv []string,
	plugin *pluginCache, limits ContainerLimits) *containerRunner {
	if runtime == "" {
		runtime = "docker"
	}
	if pullPolicy == "" {
		pullPolicy = "missing"
	}
	return &containerRunner{
		runtime:       runtime,
		pullPolicy:    pullPolicy,
		requireDigest: requireDigest,
		baseEnv:       baseEnv,
		plugin:        plugin,
		limits:        limits,
	}
}

// Run executes spec's tool inside spec.Image, mounting the paths the tool references and, for
// Ansible, the events sidecar, so the run behaves like a host run while staying isolated. A canceled
// context kills the container by name so a stopped run does not leak a container.
func (c *containerRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	terraform := isTerraformTool(spec.Tool)
	if terraform && !spec.DryRun && spec.PlanFile != "" && spec.AddSecrets != nil {
		// Rendered first so the masker holds the plan's sensitive values before the apply prints
		// anything. A plan that will not render is left for the apply to refuse.
		if rendered, err := c.showPlan(ctx, spec, spec.PlanFile, out); err == nil {
			spec.AddSecrets(PlanSensitiveValues(rendered))
		}
	}
	res, err := c.runBuilt(ctx, spec, out, buildContainerPlan)
	if terraform && spec.DryRun && spec.PlanOut != "" && err == nil && res.ExitCode == 0 {
		c.readPlan(ctx, spec, out, &res)
	}
	return res, err
}

// readPlan reads back the plan a containerized dry run saved to spec.PlanOut, which sits in a host
// directory mounted into the container, and renders it as JSON in a second container, storing both
// on res. A plan that was not saved, or that will not render, is left off.
func (c *containerRunner) readPlan(ctx context.Context, spec Spec, out io.Writer, res *Result) {
	plan, err := os.ReadFile(spec.PlanOut)
	if err != nil || len(plan) == 0 {
		return
	}
	res.PlanFile = plan
	rendered, err := c.showPlan(ctx, spec, spec.PlanOut, out)
	if err != nil {
		return
	}
	if spec.AddSecrets != nil {
		spec.AddSecrets(PlanSensitiveValues(rendered))
	}
	res.PlanJSON = rendered
}

// showPlan renders a saved plan file as JSON inside spec's image. Its standard output is captured
// and never reaches out, since it carries the plan's values in the clear.
func (c *containerRunner) showPlan(ctx context.Context, spec Spec, planFile string,
	out io.Writer) ([]byte, error) {
	if err := c.validateRunImage(spec.Image); err != nil {
		return nil, err
	}
	plan, cleanup, err := buildShowPlan(spec, planFile)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	var captured cappedCapture
	res, err := c.runPlanTo(ctx, spec, plan, &captured, out)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%w: show exited %d", ErrLaunch, res.ExitCode)
	}
	return captured.bytes()
}

// runBuilt executes spec inside spec.Image with the container plan build produces for it. Run builds
// the tool's own plan, and a module download builds the plan that runs only the tool's get, so both
// share the image checks, the registry login, the environment file, the limits, and the cleanup.
func (c *containerRunner) runBuilt(ctx context.Context, spec Spec, out io.Writer,
	build func(Spec) (containerPlan, func(), error)) (Result, error) {
	if err := c.validateRunImage(spec.Image); err != nil {
		return Result{ExitCode: -1}, err
	}
	plan, cleanup, err := build(spec)
	// Deferred before the error is checked. The plan writes temp files for some tools and returns a
	// usable cleanup even when it then fails, so checking first and deferring after left those files
	// behind on exactly the path where something already went wrong.
	defer cleanup()
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	return c.runPlan(ctx, spec, plan, out)
}

// runPlan runs plan inside spec's image: the registry login, the environment file, the mounts, and
// the cancellation handling every container run shares.
func (c *containerRunner) runPlan(ctx context.Context, spec Spec, plan containerPlan, out io.Writer) (Result, error) {
	return c.runPlanTo(ctx, spec, plan, out, out)
}

// runPlanTo is runPlan with the container's standard output sent to stdout and everything else,
// standard error and the runtime's own messages, sent to out.
func (c *containerRunner) runPlanTo(ctx context.Context, spec Spec, plan containerPlan, stdout,
	out io.Writer) (Result, error) {
	// A registry login is scoped to this run.
	//
	// The runtime writes the credential into its config directory, and that directory was the
	// executor's own, shared by every run on the machine. One project's private-image credential
	// therefore stayed on disk after its run and authenticated every later pull, so a project with no
	// credential of its own could pull from a registry it was never given access to. A per-run
	// directory means the credential exists for the length of the run and is removed with it.
	runEnv := c.baseEnv
	if spec.RegistryUsername != "" {
		configDir, cleanupConfig, err := newRuntimeConfigDirIn(spec.RunDir)
		if err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
		}
		defer cleanupConfig()
		runEnv = append(append([]string(nil), c.baseEnv...), "DOCKER_CONFIG="+configDir,
			"REGISTRY_AUTH_FILE="+filepath.Join(configDir, "config.json"))
		if err := c.login(ctx, spec, runEnv, out); err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("%w: registry login: %w", ErrLaunch, err)
		}
	}

	envFile, cleanupEnv, err := c.writeEnvFile(spec, plan.extraEnv)
	// Same ordering, and it matters more here: this file holds every resolved environment credential
	// in the clear. Returning before the defer was registered left it in the temp directory until the
	// operating system cleared it, which on most hosts is a reboot.
	defer cleanupEnv()
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	name := containerName()
	args, err := c.runArgs(spec, plan, name, envFile)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	cmd := exec.CommandContext(ctx, c.runtime, args...)
	cmd.Stdout = stdout
	cmd.Stderr = out
	// The same config the login wrote to, so the pull this command performs can see it.
	cmd.Env = runEnv
	configureContainerClient(cmd)
	// The container runs under the daemon, not under this process, so an executor killed outright
	// left it running the play with nobody to remove it. The shim the client runs under stops and
	// removes it by name when this process dies, the way a cancel does below.
	orphaned := [][]string{
		{cmd.Path, "stop", "--time", strconv.Itoa(int(containerStopGrace / time.Second)), name},
		{cmd.Path, "rm", "-f", name},
	}

	// A canceled run must stop the container itself: killing the client leaves the container running
	// under the daemon, so remove it by name. A cancel during a slow image pull can land before the
	// daemon has created the container, so retry a few times to catch one that appears just after,
	// using rm -f so a container in any state, created or running, is both killed and removed.
	// Two channels rather than one. "killed" says the run finished on its own so the remover need
	// never start; "removed" says the remover has finished. Closing a single channel in a defer meant
	// the remover was told to stop the instant cmd.Run returned, and on a cancel that is immediate,
	// because the client is SIGKILLed by the context: the loop below aborted after its first attempt
	// and every retry was dead code. A transient failure then left the container running the playbook
	// against production while the run was recorded as canceled, and --rm erased it afterward.
	killed := make(chan struct{})
	removed := make(chan struct{})
	go func() {
		defer close(removed)
		select {
		case <-ctx.Done():
		case <-killed:
			return
		}
		// A grace period first, so a tool holding a lock can put it down. The identical run on the
		// host gets processKillGrace after SIGTERM; in a container it got none, so a canceled
		// terraform died holding its state lock and every later plan or apply blocked until somebody
		// ran force-unlock by hand. A stop that fails falls through to the removal below.
		stop := exec.Command(c.runtime, "stop", "--time",
			strconv.Itoa(int(containerStopGrace/time.Second)), name)
		_ = stop.Run()
		for attempt := 0; attempt < containerKillAttempts; attempt++ {
			if err := exec.Command(c.runtime, "rm", "-f", name).Run(); err == nil {
				return
			}
			// The wait is not interruptible by the run finishing: the container outlives the client,
			// so a removal that has not succeeded yet still has to be retried.
			time.Sleep(containerKillInterval)
		}
	}()
	defer func() {
		close(killed)
		// On a cancel the remover is already working, so wait for it rather than returning while a
		// container may still be running. Bounded, so a wedged daemon delays the result instead of
		// parking this goroutine forever.
		if ctx.Err() != nil {
			select {
			case <-removed:
			case <-time.After(containerRemoveWait):
			}
		}
	}()

	runErr := runSupervised(cmd, orphaned)
	if runErr == nil {
		return Result{ExitCode: 0, ImageDigest: c.pulledDigest(spec.Image, runEnv)}, nil
	}
	if ctx.Err() != nil {
		return Result{ExitCode: -1}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code := exitErr.ExitCode()
		digest := c.pulledDigest(spec.Image, runEnv)
		// A Terraform or OpenTofu dry run uses plan -detailed-exitcode: exit 2 is a clean plan with
		// pending changes, which is drift, not a failure.
		if spec.DryRun && code == 2 && isTerraformTool(spec.Tool) {
			return Result{ExitCode: 0, Drift: true, ImageDigest: digest}, nil
		}
		return Result{ExitCode: code, ImageDigest: digest}, nil
	}
	return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, runErr)
}

// inspectTimeout bounds the runtime's answer about which image a run used.
const inspectTimeout = 10 * time.Second

// pulledDigest returns the digest of the image a container run executed in. A reference pinned by
// digest is that digest: the runtime pulls content by its digest and refuses anything else. A tag is
// asked of the runtime, which names the digest of the image it holds for the tag, the one the run
// just pulled and ran. It returns the empty string when the runtime cannot say, which the outcome
// records as no digest rather than a guess.
func (c *containerRunner) pulledDigest(image string, env []string) string {
	if _, digest, ok := strings.Cut(image, "@"); ok && strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.runtime, "image", "inspect", "--format", "{{json .RepoDigests}}",
		image)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var repoDigests []string
	if err := json.Unmarshal(out, &repoDigests); err != nil {
		return ""
	}
	name := image
	if r, perr := imageref.Parse(image); perr == nil {
		name = r.Name
	}
	pick := ""
	for _, rd := range repoDigests {
		repo, digest, ok := strings.Cut(rd, "@")
		if !ok || !strings.HasPrefix(digest, "sha256:") {
			continue
		}
		if repo == name {
			return digest
		}
		if pick == "" {
			pick = digest
		}
	}
	return pick
}

// containerHome is the home directory a containerized tool is given. It is inside the container's
// own filesystem rather than a mount, so nothing a tool writes there reaches the host.
const containerHome = "/tmp"

// hasEnvName reports whether env already assigns name, so a caller that set one keeps it.
func hasEnvName(env []string, name string) bool {
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == name {
			return true
		}
	}
	return false
}

// runArgs builds the container run argument list from the plan: resource caps, the working
// directory, an env file for variables and secrets, a bind mount for every host path the plan
// references plus the events sidecar for Ansible, the image, and the tool command to run inside it.
func (c *containerRunner) runArgs(spec Spec, plan containerPlan, name, envFile string) ([]string, error) {
	args := []string{"run", "--rm", "--name", name, "--pull", c.pullPolicy}
	args = append(args, c.limits.args()...)
	// Run as the host executor's own uid and gid. The plugin dir (0700), the callback config and
	// inline script files (0600), the credential files, and the events sidecar are all owned by this
	// uid and kept private. Without a matching --user the in-container process runs as a different uid
	// and silently cannot read the callback config or write the events sidecar, so a run's events and
	// summaries are lost with no error. Matching the uid keeps every secret-bearing file at 0600
	// rather than loosening it. The guard skips Windows, where Getuid returns -1.
	if uid := os.Getuid(); uid >= 0 {
		args = append(args, "--user", fmt.Sprintf("%d:%d", uid, os.Getgid()))
		// Running as that uid is what makes a home directory necessary. The uid belongs to the host
		// and almost never has a passwd entry in somebody else's image, so the runtime sets HOME to
		// "/", which the container's own root filesystem will not let it write. Ansible creates
		// $HOME/.ansible/tmp before it does anything else and exits 5 with a permission error, so
		// every containerized play failed on an image that does not happen to carry this uid, which
		// is nearly all of them. A writable home costs nothing: the container is removed on exit,
		// so nothing in it outlives the run.
		if !hasEnvName(spec.Env, "HOME") {
			args = append(args, "--env", "HOME="+containerHome)
		}
	}
	if plan.workdir != "" {
		args = append(args, "-w", plan.workdir)
	}

	mounts := newMountSet()
	mounts.private = spec.RunFilesRoot
	var addErr error
	addMount := func(path string, ro bool) {
		if addErr == nil {
			addErr = mounts.add(path, ro)
		}
	}
	for _, m := range plan.mounts {
		addMount(m.path, !m.writable)
	}
	// The environment, credentials and all, is mounted and sourced inside the container rather than
	// passed with --env-file. --env-file copies every value into the container's Config.Env, which
	// docker inspect returns for the life of the container, so a resolved secret was readable by
	// anything that could inspect the run. Mounting the file read-only and sourcing it in a shell
	// wrapper keeps the values out of Config.Env; inspect shows only the mount path. The file is 0600
	// and the container runs as its owner (see the --user flag above), so nothing else can read it.
	if envFile != "" {
		addMount(envFile, true)
	}
	if spec.EventsPath != "" {
		dir, err := c.plugin.ensure()
		if err != nil {
			return nil, err
		}
		addMount(dir, true)
		// The plugin writes NDJSON into the sidecar, which the host tails, so it mounts writable.
		addMount(spec.EventsPath, false)
	}
	if addErr != nil {
		return nil, addErr
	}
	// The run's private directory is an in-memory filesystem inside the container, mounted at the
	// path it has on the host. The files staged for the run are bind mounted into it read-only, one
	// by one, and everything a tool writes there, the cloud tool state pointed into it above all,
	// stays in the container's memory: it never reaches the host's disk, including the container's
	// own writable layer, which a crashed daemon can leave behind. The host side of the directory is
	// removed by the executor as for any run, so nothing here waits on the container dying.
	if spec.RunDir != "" {
		tmpfs, err := c.secretsMount(spec.RunDir)
		if err != nil {
			return nil, err
		}
		args = append(args, "--tmpfs", tmpfs)
	}
	args = append(args, mounts.args()...)

	args = append(args, spec.Image)
	if envFile != "" {
		return append(args, sourceEnvArgv(envFile, plan.argv)...), nil
	}
	return append(args, plan.argv...), nil
}

// sourceEnvArgv wraps the tool argv so the container sources the mounted env file before running it.
// The file holds shell-safe "export KEY='...'" lines, so a value with a space, a quote, or a dollar
// sign survives intact. $0 is sh, $1 is the env path; sourcing it exports the run's environment,
// shift drops the path, and exec replaces the shell with the tool argv verbatim. This is why a
// container image must carry a POSIX shell, which every built-in tool's image already does.
func sourceEnvArgv(envFile string, argv []string) []string {
	wrapper := []string{"sh", "-c", `. "$1"; shift; exec "$@"`, "sh", envFile}
	return append(wrapper, argv...)
}

// writeEnvFile writes the run's environment, the tool's extra environment, and, for Ansible, the
// callback variables to a temp file passed as --env-file so secret values never appear on the
// command line. It returns the path and a cleanup.
func (c *containerRunner) writeEnvFile(spec Spec, extraEnv []string) (string, func(), error) {
	env := append([]string{}, spec.Env...)
	env = append(env, extraEnv...)
	if spec.EventsPath != "" {
		dir, err := c.plugin.ensure()
		if err != nil {
			return "", func() {}, err
		}
		env = append(env, callbackEnv(dir, spec.EventsPath)...)
	}
	env = append(env, factCacheEnv(spec.FactCacheDir)...)
	// No environment means no file and no shell wrapper: a run that injects nothing runs the tool
	// directly, so a container image without a shell is only a constraint for a run that has env to
	// source, which in practice is every run that carries a credential.
	lines := make([]string, 0, len(env))
	for _, kv := range env {
		if line := shellExport(kv); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "", func() {}, nil
	}

	f, err := os.CreateTemp(spec.RunDir, "switchtender-env-*")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", cleanup, err
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		_ = f.Close()
		return "", cleanup, err
	}
	if err := f.Close(); err != nil {
		return "", cleanup, err
	}
	return path, cleanup, nil
}

// shellExport turns a KEY=VALUE environment entry into a POSIX sh statement that exports it, with the
// value single-quoted so any character in it, a space, a dollar sign, a backtick, a newline, is taken
// literally. Each embedded single quote is closed, escaped, and reopened, the standard way to place
// an arbitrary string inside single quotes. An entry without an '=' is not an assignment and is
// skipped. The result is sourced inside the container, which is what keeps secrets out of the
// command line and out of the container's inspectable environment.
func shellExport(kv string) string {
	key, value, ok := strings.Cut(kv, "=")
	if !ok || !validEnvName(key) {
		return ""
	}
	return "export " + key + "='" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// validEnvName reports whether a name is a POSIX shell variable name, which is what makes it safe to
// write unquoted on the left of an assignment.
//
// Quoting the value and not the name leaves half a hole. A name is not quotable, since quoting it
// would stop it being an assignment at all, so a name carrying a shell metacharacter has to be
// refused instead. Names are not always the product's own: a custom credential type lets an operator
// choose the variable a secret injects into, and an import reads those types out of a file from
// another system, so the name reaching this line is not guaranteed to be well formed.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// login authenticates to the image's registry so a private execution environment can be pulled. The
// password is fed on stdin, never as an argument.
func (c *containerRunner) login(ctx context.Context, spec Spec, env []string, out io.Writer) error {
	args := []string{"login"}
	if host := registryHost(spec.Image); host != "" {
		args = append(args, host)
	}
	args = append(args, "-u", spec.RegistryUsername, "--password-stdin")
	cmd := exec.CommandContext(ctx, c.runtime, args...)
	cmd.Stdin = strings.NewReader(spec.RegistryPassword)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = env
	return cmd.Run()
}

// newRuntimeConfigDir makes a private config directory for one run's registry login in the
// temporary directory and returns it with a cleanup that removes it.
func newRuntimeConfigDir() (string, func(), error) {
	return newRuntimeConfigDirIn("")
}

// newRuntimeConfigDirIn makes a private config directory for one run's registry login inside
// parent, the temporary directory when parent is empty, and returns it with a cleanup that removes
// it. Both the Docker and Podman variable names point at it, so the login lands there whichever
// runtime is configured. The runtime writes the registry password into it, so the run's private
// directory is the parent, where a crash leaves it to the sweep rather than to the temporary
// directory's cleaner.
func newRuntimeConfigDirIn(parent string) (string, func(), error) {
	dir, err := os.MkdirTemp(parent, "switchtender-registry-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create registry config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("secure registry config dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// mountSet collects unique host paths to bind mount into the container at the same path.
type mountSet struct {
	// seen tracks paths already added so overlapping references mount once.
	seen map[string]bool
	// specs holds the ordered docker -v arguments.
	specs []string
	// private is the run-files root, inside which a path is mountable wherever the root sits.
	private string
}

// newMountSet returns an empty mount set.
func newMountSet() *mountSet {
	return &mountSet{seen: make(map[string]bool)}
}

// add records a bind mount for path at the same path inside the container, read-only when ro is
// set. Empty and duplicate paths are ignored. It returns ErrForbiddenMount when the path would
// expose a sensitive host location.
func (m *mountSet) add(path string, ro bool) error {
	if path == "" || m.seen[path] {
		return nil
	}
	if err := checkMountPath(path); err != nil && !insidePrivateRoot(m.private, path) {
		return err
	}
	m.seen[path] = true
	spec := path + ":" + path
	if ro {
		spec += ":ro"
	}
	m.specs = append(m.specs, "-v", spec)
	return nil
}

// sensitiveMountTrees are host directories that must not be bind mounted, nor any path beneath
// them. Nothing a run legitimately needs lives here: a project checkout, a temp file, and a
// credential file are all written somewhere else, so the whole tree is refused rather than the
// directory alone.
//
// Blocking only the directory was not containment. Subpaths were deliberately allowed so a checkout
// under /var or /home could still be mounted, but that reasoning does not extend to /etc or /root,
// and the exact-match list let the interesting children straight through: /etc blocked while
// /etc/shadow passed, /root blocked while /root/.ssh passed. A run names the file it wants, never
// the directory above it, so the blocklist stopped the mount nobody was trying to make.
var sensitiveMountTrees = []string{
	"/proc", "/sys", "/dev", "/boot", "/root", "/etc",
	// The container runtime's own socket lives under these, and handing a container the socket hands
	// it the host: it can start a second container with the whole filesystem mounted and no limits.
	"/run", "/var/run", "/var/lib/docker", "/var/lib/containerd",
}

// sensitiveMountRoots are host directories that must never be bind mounted whole into a container,
// but whose subpaths stay allowed because a project checkout, a temp file, or the state directory
// legitimately lives under one of them. Mounting the directory itself would hand the container the
// host's configuration, secrets, or entire filesystem.
//
// The entries are written in lower case because the lookup folds case, so the macOS home root is
// "/users" here and matches whichever way a run spells it.
var sensitiveMountRoots = map[string]bool{
	"/": true, "/usr": true, "/bin": true, "/sbin": true, "/lib": true,
	"/lib64": true, "/var": true, "/home": true, "/users": true,
}

// sensitiveMountNames are directory names that carry credentials wherever they appear. They sit
// under a user's home, which cannot be refused as a tree because a checkout and the state directory
// live there too, so they are matched by name at any depth instead.
var sensitiveMountNames = map[string]bool{
	".ssh": true, ".aws": true, ".kube": true, ".docker": true, ".gnupg": true,
	".azure": true, ".config/gcloud": true,
}

// maxMountLinkHops bounds the symlink walk a mount path is judged over. A pair of links pointing
// at each other never resolves, so the walk stops rather than spinning, and a path with links left
// after this many hops is beyond anything a checkout legitimately builds.
const maxMountLinkHops = 40

// checkMountPath rejects a host path that would expose a sensitive host location to the container.
// An empty path is not a mount and passes.
//
// Every path the symlinks along it resolve through is judged, not only the name the run wrote. The
// spelling check alone was satisfied by a name inside the checkout that is really a link to /etc,
// /root/.ssh, or the filesystem root: the string is ordinary, the directory is not, and the
// container runtime resolves the source for real and bind mounts the target. This is the mount-side
// twin of the containment toolWorkDir already performs, and a link committed to a project's own
// repository is enough to place one, since the Ansible plan mounts the playbook directory, the
// inventory, the private key, and every vault password path exactly as they were given.
func checkMountPath(path string) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	for _, candidate := range mountPathChain(clean) {
		if !forbiddenMountPath(candidate) {
			continue
		}
		if candidate != clean {
			return fmt.Errorf("%w: %s: resolves to %s", ErrForbiddenMount, clean, candidate)
		}
		return fmt.Errorf("%w: %s", ErrForbiddenMount, clean)
	}
	return nil
}

// forbiddenMountPath reports whether one already-resolved path names a sensitive host location.
//
// The comparison folds case, because the filesystems this runs on do: macOS and Windows both hand
// /Users/ops/.SSH/id_rsa to a caller who asked for .ssh, so matching the names byte for byte handed
// out exactly the files the guard exists to withhold. Folding everywhere rather than only where the
// filesystem folds costs a run nothing, since no run needs a directory differing from a credential
// store in case alone, and it means the guard does not depend on knowing which filesystem a path is
// on to reach the same verdict.
func forbiddenMountPath(path string) bool {
	lower := strings.ToLower(path)
	if sensitiveMountRoots[lower] {
		return true
	}
	for _, tree := range sensitiveMountTrees {
		if lower == tree || strings.HasPrefix(lower, tree+"/") {
			return true
		}
	}
	// Matched on the components rather than the whole string, so a directory merely ending in one of
	// these names is not refused and one buried mid-path still is.
	parts := strings.Split(lower, "/")
	for i, part := range parts {
		if sensitiveMountNames[part] {
			return true
		}
		if i > 0 && sensitiveMountNames[parts[i-1]+"/"+part] {
			return true
		}
	}
	// Any unix socket, not only docker.sock. The runtimes this executes under name theirs
	// differently, podman.sock, containerd.sock, crio.sock, and a socket is never something an
	// execution environment needs mounted, so the whole class is refused rather than a list of names
	// that has to keep up with the runtimes.
	return strings.HasSuffix(lower, ".sock")
}

// mountPathChain returns clean followed by every path the symlinks along it resolve through. Each
// hop is judged in its own right rather than only the final destination, because the destination is
// often a further link on the host itself: /etc is a link to /private/etc on macOS, so a guard that
// looked only at where the walk ended would never see the /etc the blocklist names.
func mountPathChain(clean string) []string {
	chain := []string{clean}
	for range maxMountLinkHops {
		next, ok := followFirstLink(chain[len(chain)-1])
		if !ok {
			break
		}
		chain = append(chain, next)
	}
	return chain
}

// followFirstLink replaces the shallowest symlinked component of path with what it points at and
// reports whether it found one. Resolving from the outside in is the order the kernel resolves in,
// so every path the walk passes through is a path the mount really reaches. A component that cannot
// be read stops the walk: nothing below a path that does not exist can be a link either.
func followFirstLink(path string) (string, bool) {
	parts := strings.Split(path, "/")
	prefix := ""
	if strings.HasPrefix(path, "/") {
		prefix = "/"
	}
	for i, part := range parts {
		if part == "" {
			continue
		}
		prefix = filepath.Join(prefix, part)
		info, err := os.Lstat(prefix)
		if err != nil {
			return "", false
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(prefix)
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(prefix), target)
		}
		return filepath.Join(target, filepath.Join(parts[i+1:]...)), true
	}
	return "", false
}

// imageRefPattern matches a conservative container image reference: it must start with an
// alphanumeric character, so the container CLI cannot read it as a flag, and hold only characters
// that appear in registries, repositories, tags, and digests.
var imageRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)

// validateImage reports whether image is a well-formed, safe container reference. It returns
// ErrNoImage when empty and ErrBadImage when malformed.
func validateImage(image string) error {
	if image == "" {
		return ErrNoImage
	}
	if len(image) > 512 || strings.Contains(image, "..") || !imageRefPattern.MatchString(image) {
		return fmt.Errorf("%w: %q", ErrBadImage, image)
	}
	return nil
}

// validateRunImage confirms image is a well-formed reference and, when the runner requires digest
// pinning, that it names an immutable @sha256: digest rather than a mutable tag. It returns
// ErrUnpinnedImage when pinning is required and the reference is tag-only or unpinned.
func (c *containerRunner) validateRunImage(image string) error {
	if err := validateImage(image); err != nil {
		return err
	}
	if c.requireDigest && !isDigestPinned(image) {
		return fmt.Errorf("%w: %q", ErrUnpinnedImage, image)
	}
	return nil
}

// isDigestPinned reports whether image is pinned to an immutable content digest, meaning it carries
// an @sha256: segment, rather than a mutable tag.
func isDigestPinned(image string) bool {
	return strings.Contains(image, "@sha256:")
}

// args returns the accumulated docker -v arguments.
func (m *mountSet) args() []string {
	return m.specs
}

// registryHost returns the registry portion of a container image reference, or empty for Docker
// Hub. A first path segment containing a dot, a colon, or the localhost name is a registry host.
func registryHost(image string) string {
	before, _, ok := strings.Cut(image, "/")
	if !ok {
		return ""
	}
	first := before
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return ""
}

// containerName returns a unique container name so a run's container can be killed on cancel.
func containerName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ym-container"
	}
	return "ym-" + hex.EncodeToString(b[:])
}
