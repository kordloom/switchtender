package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AnsibleCoreReporter reports the ansible-core version installed where this process runs Ansible.
type AnsibleCoreReporter interface {
	// AnsibleCoreVersion returns the version, such as 2.18.1, or ErrAnsibleMissing when Ansible
	// is not installed.
	AnsibleCoreVersion(ctx context.Context) (string, error)
}

// InventoryReader reads inventory files the way a run's own Ansible will read them: with the run's
// environment and its project's ansible.cfg, on the host or inside the run's image. It is how the
// executor cross-checks an inventory the native engine resolved before a play runs against it.
type InventoryReader interface {
	// ReadInventories returns the ansible-inventory --list JSON for each file named in names, read
	// from dir one file at a time, and the ansible-core version that read them.
	ReadInventories(ctx context.Context, spec Spec, dir string,
		names []string) ([][]byte, string, error)
}

// ErrAnsibleMissing is returned when Ansible's commands are not installed where they are needed.
var ErrAnsibleMissing = errors.New("ansible-inventory is not installed")

// versionCacheTTL bounds how long a reported ansible-core version is reused, so doctor and every
// composed launch do not each start a Python process, while an upgrade is noticed within minutes.
const versionCacheTTL = 5 * time.Minute

// versionTimeout bounds one ansible-inventory --version.
const versionTimeout = 30 * time.Second

// coreBanner matches the version in an Ansible command's --version banner.
var coreBanner = regexp.MustCompile(`\[core ([0-9]+\.[0-9]+[0-9A-Za-z.+-]*)\]`)

// versionCache holds the last version an Ansible command reported.
type versionCache struct {
	// mu guards the fields below.
	mu sync.Mutex
	// version is the last version read.
	version string
	// err is the last error reading it.
	err error
	// at is when it was read.
	at time.Time
}

// hostVersions caches the host's ansible-core version for the process.
var hostVersions versionCache

// AnsibleCoreVersion returns the ansible-core version of the ansible-inventory on PATH.
func (a *ansibleRunner) AnsibleCoreVersion(ctx context.Context) (string, error) {
	hostVersions.mu.Lock()
	defer hostVersions.mu.Unlock()
	if !hostVersions.at.IsZero() && time.Since(hostVersions.at) < versionCacheTTL {
		return hostVersions.version, hostVersions.err
	}
	hostVersions.version, hostVersions.err = ansibleCoreVersion(ctx, a.baseEnv, "")
	hostVersions.at = time.Now()
	return hostVersions.version, hostVersions.err
}

// ansibleCoreVersion runs ansible-inventory --version in env and dir and reads the version.
func ansibleCoreVersion(ctx context.Context, env []string, dir string) (string, error) {
	bin, err := exec.LookPath(defaultInventoryBinary)
	if err != nil {
		return "", ErrAnsibleMissing
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s --version: %w", ErrLaunch, defaultInventoryBinary, err)
	}
	m := coreBanner.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("%w: %s --version reported no ansible-core version", ErrLaunch,
			defaultInventoryBinary)
	}
	return string(m[1]), nil
}

// checkEnv is the environment an inventory cross-check reads with: the run's own, with its
// project's ansible.cfg named explicitly, since the check reads from a directory of its own rather
// than the project, and with a file that does not parse reported as a failure rather than read as
// an empty inventory.
func checkEnv(base []string, spec Spec) []string {
	env := append(append([]string{}, base...), spec.Env...)
	if !hasEnvName(env, "ANSIBLE_CONFIG") && spec.Dir != "" {
		if cfg := filepath.Join(spec.Dir, "ansible.cfg"); fileExists(cfg) {
			env = append(env, "ANSIBLE_CONFIG="+cfg)
		}
	}
	return append(env, "ANSIBLE_INVENTORY_UNPARSED_FAILED=true", "ANSIBLE_NOCOLOR=1")
}

// fileExists reports whether path names a regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// maxCheckStderr bounds how much of a failed read's error output an error carries.
const maxCheckStderr = 2048

// ReadInventories reads each file with the host's ansible-inventory.
func (a *ansibleRunner) ReadInventories(ctx context.Context, spec Spec, dir string,
	names []string) ([][]byte, string, error) {
	bin, err := exec.LookPath(defaultInventoryBinary)
	if err != nil {
		return nil, "", ErrAnsibleMissing
	}
	env := checkEnv(a.baseEnv, spec)
	out := make([][]byte, len(names))
	for i, name := range names {
		cmd := exec.CommandContext(ctx, bin, "-i", filepath.Join(dir, name), "--list")
		cmd.Env = env
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			return nil, "", fmt.Errorf("%w: %s %s: %w: %s", ErrLaunch, defaultInventoryBinary, name,
				err, clipTail(stderr.String()))
		}
		out[i] = data
	}
	version, err := ansibleCoreVersion(ctx, env, dir)
	if err != nil {
		return nil, "", err
	}
	return out, version, nil
}

// clipTail returns at most the last maxCheckStderr bytes of s, trimmed.
func clipTail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxCheckStderr {
		s = s[len(s)-maxCheckStderr:]
	}
	return s
}

// ReadInventories reads each file with the Ansible the run will execute with: inside the run's
// image when it names one, and on the host otherwise.
func (t *toolRouter) ReadInventories(ctx context.Context, spec Spec, dir string,
	names []string) ([][]byte, string, error) {
	if spec.Image == "" {
		return t.ansibleRunner.ReadInventories(ctx, spec, dir, names)
	}
	if !t.allowContainer {
		return nil, "", ErrContainerDisabled
	}
	return t.container.readInventories(ctx, spec, dir, names)
}

// inventoryCheckScript reads each file named in its arguments with ansible-inventory, writing each
// listing beside the file, and then records the ansible-core version, all inside the image.
const inventoryCheckScript = `set -e
for f in "$@"; do ansible-inventory -i "$f" --list --output "$f.json"; done
ansible-inventory --version > version.txt`

// readInventories runs the cross-check inside the run's image, with the check directory mounted
// writable and the project mounted read-only for its ansible.cfg.
func (c *containerRunner) readInventories(ctx context.Context, spec Spec, dir string,
	names []string) ([][]byte, string, error) {
	check := spec
	check.Tool = "ansible"
	check.EventsPath = ""
	check.FactCacheDir = ""
	check.ExtraVars = nil
	check.ExtraVarsFiles = nil
	check.Env = checkEnv(nil, spec)
	argv := append([]string{"sh", "-c", inventoryCheckScript, "sh"}, names...)
	mounts := []planMount{{path: dir, writable: true}}
	if spec.Dir != "" {
		mounts = append(mounts, planMount{path: spec.Dir})
	}
	if err := c.validateRunImage(check.Image); err != nil {
		return nil, "", err
	}
	plan := containerPlan{argv: argv, workdir: dir, mounts: mounts}
	for _, f := range spec.CredentialFiles {
		plan.mounts = append(plan.mounts, planMount{path: f})
	}
	var output bytes.Buffer
	res, err := c.runPlan(ctx, check, plan, &output)
	if err != nil {
		return nil, "", err
	}
	if res.ExitCode != 0 {
		return nil, "", fmt.Errorf("%w: ansible-inventory in %s exited %d: %s", ErrLaunch,
			spec.Image, res.ExitCode, clipTail(output.String()))
	}
	out := make([][]byte, len(names))
	for i, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			return nil, "", fmt.Errorf("%w: read the listing of %s: %w", ErrLaunch, name, err)
		}
		out[i] = data
	}
	banner, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	if err != nil {
		return nil, "", fmt.Errorf("%w: read the ansible-core version: %w", ErrLaunch, err)
	}
	m := coreBanner.FindSubmatch(banner)
	if m == nil {
		return nil, "", fmt.Errorf("%w: ansible-inventory in %s reported no ansible-core version",
			ErrLaunch, spec.Image)
	}
	return out, string(m[1]), nil
}
