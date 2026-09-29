package roundhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	if err := c.validateRunImage(spec.Image); err != nil {
		return Result{ExitCode: -1}, err
	}
	plan, cleanup, err := buildContainerPlan(spec)
	// Deferred before the error is checked. The plan writes temp files for some tools and returns a
	// usable cleanup even when it then fails, so checking first and deferring after left those files
	// behind on exactly the path where something already went wrong.
	defer cleanup()
	if err != nil {
		return Result{ExitCode: -1}, err
	}

	// A registry login is scoped to this run.
	//
	// The runtime writes the credential into its config directory, and that directory was the
	// executor's own, shared by every run on the machine. One project's private-image credential
	// therefore stayed on disk after its run and authenticated every later pull, so a project with no
	// credential of its own could pull from a registry it was never given access to. A per-run
	// directory means the credential exists for the length of the run and is removed with it.
	runEnv := c.baseEnv
	if spec.RegistryUsername != "" {
		configDir, cleanupConfig, err := newRuntimeConfigDir()
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
	cmd.Stdout = out
	cmd.Stderr = out
	// The same config the login wrote to, so the pull this command performs can see it.
	cmd.Env = runEnv

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

	runErr := cmd.Run()
	if runErr == nil {
		return Result{ExitCode: 0}, nil
	}
	if ctx.Err() != nil {
		return Result{ExitCode: -1}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code := exitErr.ExitCode()
		// A Terraform or OpenTofu dry run uses plan -detailed-exitcode: exit 2 is a clean plan with
		// pending changes, which is drift, not a failure.
		if spec.DryRun && code == 2 && isTerraformTool(spec.Tool) {
			return Result{ExitCode: 0, Drift: true}, nil
		}
		return Result{ExitCode: code}, nil
	}
	return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, runErr)
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

	f, err := os.CreateTemp("", "switchtender-env-*")
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

// newRuntimeConfigDir makes a private config directory for one run's registry login and returns it
// with a cleanup that removes it. Both the Docker and Podman variable names point at it, so the
// login lands there whichever runtime is configured.
func newRuntimeConfigDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "switchtender-registry-*")
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
	if err := checkMountPath(path); err != nil {
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
var sensitiveMountRoots = map[string]bool{
	"/": true, "/usr": true, "/bin": true, "/sbin": true, "/lib": true,
	"/lib64": true, "/var": true, "/home": true, "/Users": true,
}

// sensitiveMountNames are directory names that carry credentials wherever they appear. They sit
// under a user's home, which cannot be refused as a tree because a checkout and the state directory
// live there too, so they are matched by name at any depth instead.
var sensitiveMountNames = map[string]bool{
	".ssh": true, ".aws": true, ".kube": true, ".docker": true, ".gnupg": true,
	".azure": true, ".config/gcloud": true,
}

// checkMountPath rejects a host path that would expose a sensitive host location to the container.
// An empty path is not a mount and passes.
func checkMountPath(path string) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	if sensitiveMountRoots[clean] {
		return fmt.Errorf("%w: %s", ErrForbiddenMount, clean)
	}
	for _, tree := range sensitiveMountTrees {
		if clean == tree || strings.HasPrefix(clean, tree+"/") {
			return fmt.Errorf("%w: %s", ErrForbiddenMount, clean)
		}
	}
	// Matched on the components rather than the whole string, so a directory merely ending in one of
	// these names is not refused and one buried mid-path still is.
	parts := strings.Split(clean, "/")
	for i, part := range parts {
		if sensitiveMountNames[part] {
			return fmt.Errorf("%w: %s", ErrForbiddenMount, clean)
		}
		if i > 0 && sensitiveMountNames[parts[i-1]+"/"+part] {
			return fmt.Errorf("%w: %s", ErrForbiddenMount, clean)
		}
	}
	// Any unix socket, not only docker.sock. The runtimes this executes under name theirs
	// differently, podman.sock, containerd.sock, crio.sock, and a socket is never something an
	// execution environment needs mounted, so the whole class is refused rather than a list of names
	// that has to keep up with the runtimes.
	if strings.HasSuffix(clean, ".sock") {
		return fmt.Errorf("%w: %s", ErrForbiddenMount, clean)
	}
	return nil
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
