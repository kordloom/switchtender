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
	"strings"
)

// containerRunner executes ansible-playbook inside a container image so each project can pin its
// own ansible, Python, and system dependencies independent of the host.
type containerRunner struct {
	// docker is the container CLI, docker by default.
	docker string
	// baseEnv is the environment the host CLI inherits when pulling images and logging in.
	baseEnv []string
	// plugin materializes the callback plugin on first use, shared into the container read-only.
	plugin *pluginCache
	// limits caps the memory, CPU, process count, and network of every container run.
	limits ContainerLimits
}

// newContainerRunner builds a container runner sharing the host runner's plugin cache and base
// environment, bounded by limits.
func newContainerRunner(baseEnv []string, plugin *pluginCache, limits ContainerLimits) *containerRunner {
	return &containerRunner{docker: "docker", baseEnv: baseEnv, plugin: plugin, limits: limits}
}

// Run executes the playbook inside spec.Image, mounting the checkout, inventory, credential files,
// and the events sidecar so the run behaves like a host run while staying isolated. A canceled
// context kills the container by name so a stopped run does not leak a container.
func (c *containerRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Playbook == "" {
		return Result{ExitCode: -1}, ErrNoPlaybook
	}
	if err := validateImage(spec.Image); err != nil {
		return Result{ExitCode: -1}, err
	}

	if spec.RegistryUsername != "" {
		if err := c.login(ctx, spec, out); err != nil {
			return Result{ExitCode: -1}, fmt.Errorf("%w: registry login: %w", ErrLaunch, err)
		}
	}

	envFile, cleanupEnv, err := c.writeEnvFile(spec)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	defer cleanupEnv()

	name := containerName()
	args, err := c.dockerArgs(spec, name, envFile)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	cmd := exec.CommandContext(ctx, c.docker, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = c.baseEnv

	// A canceled run must stop the container itself: killing the docker client leaves the container
	// running under the daemon, so kill it by name.
	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			kill := exec.Command(c.docker, "kill", name)
			_ = kill.Run()
		case <-killed:
		}
	}()
	defer close(killed)

	runErr := cmd.Run()
	if runErr == nil {
		return Result{ExitCode: 0}, nil
	}
	if ctx.Err() != nil {
		return Result{ExitCode: -1}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return Result{ExitCode: exitErr.ExitCode()}, nil
	}
	return Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrLaunch, runErr)
}

// dockerArgs builds the docker run argument list: mounts for every host path the run references,
// an env file for variables and secrets, the image, and the ansible-playbook command.
func (c *containerRunner) dockerArgs(spec Spec, name, envFile string) ([]string, error) {
	args := []string{"run", "--rm", "--name", name}
	args = append(args, c.limits.args()...)
	if spec.Dir != "" {
		args = append(args, "-w", spec.Dir)
	}
	args = append(args, "--env-file", envFile)

	mounts := newMountSet()
	var addErr error
	addMount := func(path string, ro bool) {
		if addErr == nil {
			addErr = mounts.add(path, ro)
		}
	}
	addMount(spec.Dir, true)
	addMount(filepath.Dir(spec.Playbook), true)
	addMount(spec.Inventory, true)
	addMount(spec.PrivateKeyPath, true)
	addMount(spec.VaultPasswordFile, true)
	for _, f := range spec.ExtraVarsFiles {
		addMount(f, true)
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

	pargs, err := playbookArgs(spec)
	if err != nil {
		return nil, err
	}
	args = append(args, spec.Image, "ansible-playbook")
	return append(args, pargs...), nil
}

// writeEnvFile writes the run's environment, plus the callback variables, to a temp file passed as
// --env-file so secret values never appear on the command line. It returns the path and a cleanup.
func (c *containerRunner) writeEnvFile(spec Spec) (string, func(), error) {
	lines := append([]string{}, spec.Env...)
	if spec.EventsPath != "" {
		dir, err := c.plugin.ensure()
		if err != nil {
			return "", func() {}, err
		}
		lines = append(lines, callbackEnv(dir, spec.EventsPath)...)
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
	if len(lines) > 0 {
		if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
			_ = f.Close()
			return "", cleanup, err
		}
	}
	if err := f.Close(); err != nil {
		return "", cleanup, err
	}
	return path, cleanup, nil
}

// login authenticates to the image's registry so a private execution environment can be pulled. The
// password is fed on stdin, never as an argument.
func (c *containerRunner) login(ctx context.Context, spec Spec, out io.Writer) error {
	args := []string{"login"}
	if host := registryHost(spec.Image); host != "" {
		args = append(args, host)
	}
	args = append(args, "-u", spec.RegistryUsername, "--password-stdin")
	cmd := exec.CommandContext(ctx, c.docker, args...)
	cmd.Stdin = strings.NewReader(spec.RegistryPassword)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = c.baseEnv
	return cmd.Run()
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

// sensitiveMountRoots are host directories that must never be bind mounted whole into a container.
// Subpaths stay allowed, since project checkouts and temp files legitimately live under some of
// these, but mounting the directory itself would hand the container the host's configuration,
// secrets, or entire filesystem.
var sensitiveMountRoots = map[string]bool{
	"/": true, "/etc": true, "/var": true, "/usr": true, "/bin": true,
	"/sbin": true, "/lib": true, "/lib64": true, "/boot": true, "/proc": true,
	"/sys": true, "/dev": true, "/root": true, "/home": true, "/Users": true,
}

// checkMountPath rejects a host path that would expose a sensitive root directory or the docker
// socket to the container. An empty path is not a mount and passes.
func checkMountPath(path string) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	if sensitiveMountRoots[clean] {
		return fmt.Errorf("%w: %s", ErrForbiddenMount, clean)
	}
	if filepath.Base(clean) == "docker.sock" {
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
