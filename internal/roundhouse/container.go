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
}

// newContainerRunner builds a container runner sharing the host runner's plugin cache and base
// environment.
func newContainerRunner(baseEnv []string, plugin *pluginCache) *containerRunner {
	return &containerRunner{docker: "docker", baseEnv: baseEnv, plugin: plugin}
}

// Run executes the playbook inside spec.Image, mounting the checkout, inventory, credential files,
// and the events sidecar so the run behaves like a host run while staying isolated. A canceled
// context kills the container by name so a stopped run does not leak a container.
func (c *containerRunner) Run(ctx context.Context, spec Spec, out io.Writer) (Result, error) {
	if spec.Playbook == "" {
		return Result{ExitCode: -1}, ErrNoPlaybook
	}
	if spec.Image == "" {
		return Result{ExitCode: -1}, ErrNoImage
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
	if spec.Dir != "" {
		args = append(args, "-w", spec.Dir)
	}
	args = append(args, "--env-file", envFile)

	mounts := newMountSet()
	mounts.add(spec.Dir, true)
	mounts.add(filepath.Dir(spec.Playbook), true)
	mounts.add(spec.Inventory, true)
	mounts.add(spec.PrivateKeyPath, true)
	mounts.add(spec.VaultPasswordFile, true)
	for _, f := range spec.ExtraVarsFiles {
		mounts.add(f, true)
	}
	if spec.EventsPath != "" {
		dir, err := c.plugin.ensure()
		if err != nil {
			return nil, err
		}
		mounts.add(dir, true)
		// The plugin writes NDJSON into the sidecar, which the host tails, so it mounts writable.
		mounts.add(spec.EventsPath, false)
	}
	args = append(args, mounts.args()...)

	args = append(args, spec.Image, "ansible-playbook")
	return append(args, playbookArgs(spec)...), nil
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

	f, err := os.CreateTemp("", "yardmaster-env-*")
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
// set. Empty and duplicate paths are ignored.
func (m *mountSet) add(path string, ro bool) {
	if path == "" || m.seen[path] {
		return
	}
	m.seen[path] = true
	spec := path + ":" + path
	if ro {
		spec += ":ro"
	}
	m.specs = append(m.specs, "-v", spec)
}

// args returns the accumulated docker -v arguments.
func (m *mountSet) args() []string {
	return m.specs
}

// registryHost returns the registry portion of a container image reference, or empty for Docker
// Hub. A first path segment containing a dot, a colon, or the localhost name is a registry host.
func registryHost(image string) string {
	slash := strings.IndexByte(image, '/')
	if slash == -1 {
		return ""
	}
	first := image[:slash]
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
