package ansibleruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	// verifyTimeout bounds the ansible --version an install verifies with.
	verifyTimeout = 2 * time.Minute
	// maxOutputTail bounds how much of pip's output an error carries.
	maxOutputTail = 4096
)

// coreVersionPattern matches the release in ansible --version's first line.
var coreVersionPattern = regexp.MustCompile(`\[core ([0-9][^\]\s]*)\]`)

// InstallOptions configures Install.
type InstallOptions struct {
	// Root is the managed runtime's directory. It is created when missing.
	Root string
	// Version is the ansible-core release or minor version to install, empty for the newest
	// supported release.
	Version string
	// Python is the interpreter to build the environment with, empty to find one on PATH.
	Python string
	// Wheels is a directory of wheels to install from instead of a package index, for an offline
	// install. A relative path is taken from the working directory. The lock's hashes are checked
	// against the wheels the same way.
	Wheels string
	// IndexURL is the package index pip downloads from instead of PyPI, such as an internal mirror.
	// The lock's hashes are checked against what it serves the same way.
	IndexURL string
	// Out receives the output of venv and pip as the install runs, nil for none.
	Out io.Writer
	// lock replaces the embedded lock Version selects. Tests set it.
	lock *Lock
}

// Result is what Install did.
type Result struct {
	// Runtime is the installed runtime, now the one in use.
	Runtime *Runtime
	// AlreadyInstalled reports that a finished runtime from the same lock was there and verified, so
	// nothing was installed.
	AlreadyInstalled bool
}

// marker is the record a finished install writes into its environment, last.
type marker struct {
	// Release is the ansible-core release installed.
	Release string `json:"ansible_core"`
	// Python is the version of the Python the environment runs, read from the environment itself.
	Python string `json:"python"`
	// LockSHA256 is the hex SHA-256 of the lock it was installed from.
	LockSHA256 string `json:"lock_sha256"`
	// InstalledAt is when the install finished and was verified.
	InstalledAt time.Time `json:"installed_at"`
}

// Install builds the managed runtime for opts.Version under opts.Root and makes it the one in use.
//
// It creates a virtual environment with a Python the release supports, installs the release's lock
// into it with pip in hash-checking mode and from prebuilt wheels only, so a file whose hash the
// lock does not record fails the install and nothing is compiled, and verifies the result by
// running its ansible --version. The interpreter and pip run isolated, with no PYTHON* or PIP_*
// variable and no pip configuration file, so no requirement, index, or code reaches the install
// from anywhere but the lock and these options.
//
// It refuses a runtime directory another account owns or can write, and never executes anything in
// an environment that account could have changed. Every install builds into a directory nothing
// else uses, writes the marker that makes it count as installed last, and only then makes it
// current, so a failed install removes what it made and leaves the runtime in use as it was.
// Installing a release that is already installed from the same lock, and still verifies, changes
// nothing but which release is in use.
func Install(ctx context.Context, opts InstallOptions) (*Result, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("%w: it runs on Linux, macOS, and other Unix systems, so a Windows "+
			"server runs Ansible through a container image", ErrUnsupported)
	}
	lock := opts.lock
	if lock == nil {
		l, err := LockFor(opts.Version)
		if err != nil {
			return nil, err
		}
		lock = l
	}
	if strings.TrimSpace(opts.Root) == "" {
		return nil, fmt.Errorf("%w: no runtime directory was given", ErrInstall)
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	wheels, err := wheelDir(opts.Wheels)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	if err := checkChain(root); err != nil {
		return nil, fmt.Errorf("%w. Install into a directory only this account or root can "+
			"write, such as /opt/switchtender/ansible", err)
	}
	unlock, err := lockRoot(root)
	if err != nil {
		return nil, fmt.Errorf("%w: lock %s: %w", ErrInstall, root, err)
	}
	defer unlock()
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	if rt := reusable(ctx, root, lock); rt != nil {
		if err := setCurrent(root, filepath.Base(rt.Dir)); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInstall, err)
		}
		rt.Current = true
		return &Result{Runtime: rt, AlreadyInstalled: true}, nil
	}
	py, err := findPython(ctx, opts.Python, lock)
	if err != nil {
		return nil, err
	}
	dir, err := freshEnv(root, lock)
	if err != nil {
		return nil, err
	}
	rt, err := build(ctx, dir, lock, py, wheels, opts.IndexURL, out)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := setCurrent(root, filepath.Base(dir)); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	rt.Current = true
	return &Result{Runtime: rt}, nil
}

// wheelDir returns the wheel directory an install reads, absolute, so pip, which runs in the new
// environment, finds the directory the caller named. It refuses anything that is not an existing
// directory, a URL included, before anything is created.
func wheelDir(wheels string) (string, error) {
	if strings.TrimSpace(wheels) == "" {
		return "", nil
	}
	abs, err := filepath.Abs(wheels)
	if err != nil {
		return "", fmt.Errorf("%w: --wheels %s: %w", ErrInstall, wheels, err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: --wheels %s is not a directory of wheels", ErrInstall, wheels)
	}
	return abs, nil
}

// reusable returns a finished environment of lock under root that is trusted and still verifies,
// the one in use first, or nil when there is none. An environment another account could have
// changed is never listed, so its commands are never run.
func reusable(ctx context.Context, root string, lock *Lock) *Runtime {
	list, err := List(root)
	if err != nil {
		return nil
	}
	var same []*Runtime
	for _, rt := range list {
		if rt.Release == lock.Release && rt.LockSHA256 == lock.SHA256() {
			same = append(same, rt)
		}
	}
	slices.SortStableFunc(same, func(a, b *Runtime) int {
		switch {
		case a.Current == b.Current:
			return 0
		case a.Current:
			return -1
		default:
			return 1
		}
	})
	for _, rt := range same {
		if verify(ctx, rt.Dir, lock.Release) == nil {
			return rt
		}
	}
	return nil
}

// freshEnv creates and returns a directory for a new environment of lock under root that nothing
// else uses: named for the lock, or, when that name is taken, the name with a random suffix.
func freshEnv(root string, lock *Lock) (string, error) {
	name := envName(lock)
	for range 8 {
		dir := filepath.Join(root, name)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%w: %w", ErrInstall, err)
		}
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			return "", fmt.Errorf("%w: %w", ErrInstall, err)
		}
		name = envName(lock) + "-" + hex.EncodeToString(suffix)
	}
	return "", fmt.Errorf("%w: no free directory name for %s under %s", ErrInstall, envName(lock),
		root)
}

// build creates the environment in dir, installs the lock into it with pip in hash-checking mode,
// verifies it, and writes its marker last.
func build(ctx context.Context, dir string, lock *Lock, py *python, wheels, indexURL string,
	out io.Writer) (*Runtime, error) {
	fmt.Fprintf(out, "Creating a Python %s environment in %s\n", py.Version, dir)
	if tail, err := runTee(ctx, out, "", py.Path, "-I", "-m", "venv", dir); err != nil {
		return nil, fmt.Errorf("%w: %s -m venv: %w: %s", ErrInstall, py.Path, err, tail)
	}
	envPython := filepath.Join(dir, "bin", "python")
	version, err := probe(ctx, envPython, pythonProbe)
	if err != nil {
		return nil, fmt.Errorf("%w: the new environment's Python did not report its version: %w",
			ErrInstall, err)
	}
	if !pythonInRange(version, lock) {
		return nil, fmt.Errorf("%w: the new environment runs Python %s, and ansible-core %s runs on "+
			"Python %s to %s", ErrPython, version, lock.Release, lock.PythonMin, lock.PythonMax)
	}
	lockPath := filepath.Join(dir, lockCopyName)
	if err := os.WriteFile(lockPath, lock.Content, 0o644); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	fmt.Fprintf(out, "Installing ansible-core %s with pip in hash-checking mode\n", lock.Release)
	tail, err := runTee(ctx, out, dir, envPython, pipArgs(lockPath, wheels, indexURL)...)
	if err != nil {
		if strings.Contains(strings.ToLower(tail), "do not match the hashes") {
			return nil, fmt.Errorf("%w: %w for ansible-core %s, so nothing was installed. The "+
				"package index or mirror served bytes the lock does not record: %s", ErrInstall,
				ErrHashMismatch, lock.Release, tail)
		}
		return nil, fmt.Errorf("%w: pip install: %w: %s", ErrInstall, err, tail)
	}
	// Built under a permissive umask, the environment would be writable by other accounts, and a
	// runtime they can change is one no run trusts.
	if err := clearGroupOtherWrite(dir); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	if err := verify(ctx, dir, lock.Release); err != nil {
		return nil, err
	}
	m := marker{Release: lock.Release, Python: version, LockSHA256: lock.SHA256(),
		InstalledAt: time.Now().UTC().Truncate(time.Second)}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, markerName), append(b, '\n')); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	return &Runtime{Release: m.Release, Python: m.Python, LockSHA256: m.LockSHA256,
		InstalledAt: m.InstalledAt, Dir: dir}, nil
}

// pipArgs returns the pip command line that installs the lock at lockPath: an isolated interpreter,
// hash-checking mode, prebuilt wheels only, no cache, from the wheel directory alone when one is
// named, and from indexURL when one is named.
func pipArgs(lockPath, wheels, indexURL string) []string {
	args := []string{"-I", "-m", "pip", "install", "--require-hashes", "--only-binary=:all:",
		"--no-cache-dir", "--no-input", "--disable-pip-version-check", "-r", lockPath}
	if wheels != "" {
		args = append(args, "--no-index", "--find-links", wheels)
	} else if indexURL != "" {
		args = append(args, "--index-url", indexURL)
	}
	return args
}

// verify runs the environment's ansible --version and confirms it reports release, and that the
// commands a run starts are there.
func verify(ctx context.Context, dir, release string) error {
	bin := filepath.Join(dir, "bin")
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(bin, "ansible"), "--version")
	cmd.Dir = dir
	cmd.Env = installEnv()
	got, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%w: %s --version: %w", ErrVerify, filepath.Join(bin, "ansible"), err)
	}
	m := coreVersionPattern.FindSubmatch(got)
	if m == nil {
		return fmt.Errorf("%w: ansible --version reported no ansible-core release", ErrVerify)
	}
	if string(m[1]) != release {
		return fmt.Errorf("%w: ansible --version reported ansible-core %s, and the lock installs %s",
			ErrVerify, m[1], release)
	}
	for _, name := range []string{"ansible-playbook", "ansible-inventory"} {
		if !isExecutable(filepath.Join(bin, name)) {
			return fmt.Errorf("%w: %s is missing from %s", ErrVerify, name, bin)
		}
	}
	return nil
}

// keptEnv names the variables an install passes on: what finds programs, the account's home, the
// locale, the temporary directory, and what reaches a package index through a proxy or with a
// private certificate authority. Every PYTHON* and PIP_* variable is left out, since PYTHONPATH and
// PYTHONHOME choose the code that runs as pip, and PIP_REQUIREMENT or a configuration file adds
// requirements of their own.
var keptEnv = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LANGUAGE", "HTTP_PROXY",
	"HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}

// installEnv returns the environment venv, pip, the interpreter probes, and verification run with:
// the variables keptEnv names and the locale's LC_ variables from this process, no pip
// configuration file, and no user site directory.
func installEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keptEnv, name) || strings.HasPrefix(name, "LC_") {
			out = append(out, kv)
		}
	}
	return append(out, "PIP_CONFIG_FILE="+os.DevNull, "PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PYTHONNOUSERSITE=1")
}

// runTee runs name with args in dir, in the install's environment, copying its combined output to
// out as it runs, and returns the tail of that output for an error to carry.
func runTee(ctx context.Context, out io.Writer, dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = installEnv()
	cmd.Stdout = io.MultiWriter(out, &buf)
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	tail := strings.TrimSpace(buf.String())
	if len(tail) > maxOutputTail {
		tail = tail[len(tail)-maxOutputTail:]
	}
	return tail, err
}
