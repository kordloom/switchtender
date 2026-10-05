// Package ansibleruntime installs, lists, removes, and locates the managed Ansible runtime: a
// Python virtual environment holding one pinned ansible-core release, installed with pip in
// hash-checking mode from a requirements lock that ships in this binary.
//
// Ansible is GPLv3, so the boundary between it and SwitchTender is a process boundary. The
// runtime's commands are only ever started as separate programs, exactly as an Ansible found on
// PATH is, and nothing here imports or links them.
package ansibleruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Where the Ansible commands a run executes come from, as doctor and a run's evidence name it.
const (
	// SourceConfigured is a directory named by --ansible-bin or SWITCHTENDER_ANSIBLE_BIN.
	SourceConfigured = "configured"
	// SourceManaged is the managed runtime that switchtender ansible install created.
	SourceManaged = "managed"
	// SourcePath is whatever the process's PATH finds.
	SourcePath = "path"
	// SourceImage is the Ansible inside the container image a run executes in.
	SourceImage = "image"
)

const (
	// BinEnv names the directory holding the Ansible commands to run, or SystemBin.
	BinEnv = "SWITCHTENDER_ANSIBLE_BIN"
	// RootEnv names the managed runtime's directory when no flag does.
	RootEnv = "SWITCHTENDER_ANSIBLE_RUNTIME_DIR"
	// SystemBin is the configured value that selects the Ansible on PATH even when a managed runtime
	// is installed.
	SystemBin = "system"
	// DirName is the managed runtime's directory under the server's data directory.
	DirName = "ansible"
	// markerName is the file a finished and verified install writes into its environment last, so
	// an environment without it is an install that never finished.
	markerName = "switchtender-runtime.json"
	// currentName is the file in the root that names the environment in use.
	currentName = "current"
	// digestLen is how many hex digits of its lock's SHA-256 an environment's directory name carries.
	digestLen = 12
	// lockCopyName is the copy of the lock an environment was installed from, kept inside it.
	lockCopyName = "switchtender-lock.txt"
)

// Commands is where the Ansible commands come from.
type Commands struct {
	// Dir holds ansible-playbook, ansible-inventory, and the other Ansible commands. It is empty
	// when PATH finds them, and when Problem is set.
	Dir string `json:"dir,omitempty"`
	// Source says how they were chosen: configured, managed, or path.
	Source string `json:"source"`
	// Release is the managed runtime's ansible-core release, empty for any other source.
	Release string `json:"release,omitempty"`
	// Root is the managed runtime's directory that was consulted, whatever the source.
	Root string `json:"runtime_dir,omitempty"`
	// Problem says why the Ansible selected cannot be used, such as a managed runtime in use that is
	// broken or that another account can write. Every command then fails rather than running
	// another Ansible nobody chose.
	Problem string `json:"problem,omitempty"`
}

// Command returns what to start the Ansible command name as: its path in Dir, or the bare name for
// PATH to find. It returns ErrUnusable when the selected Ansible cannot be used.
func (c Commands) Command(name string) (string, error) {
	if c.Problem != "" {
		return "", fmt.Errorf("%w: %s", ErrUnusable, c.Problem)
	}
	if c.Dir == "" {
		return name, nil
	}
	return filepath.Join(c.Dir, name), nil
}

// Path returns the path of the Ansible command name: inside Dir when one is set, and as PATH finds
// it otherwise. It returns ErrUnusable when the selected Ansible cannot be used and
// ErrCommandMissing when the command is not there.
func (c Commands) Path(name string) (string, error) {
	cmd, err := c.Command(name)
	if err != nil {
		return "", err
	}
	if c.Dir == "" {
		p, err := exec.LookPath(cmd)
		if err != nil {
			return "", fmt.Errorf("%w: %s is not on PATH", ErrCommandMissing, name)
		}
		return p, nil
	}
	if !isExecutable(cmd) {
		return "", fmt.Errorf("%w: %s is not in %s", ErrCommandMissing, name, c.Dir)
	}
	return cmd, nil
}

// Hold keeps a managed runtime from being removed while a command started from it runs, and
// returns the function that lets it go. Remove refuses an environment that is held. For any other
// source it holds nothing.
func (c Commands) Hold() (func(), error) {
	if c.Source != SourceManaged || c.Problem != "" || c.Dir == "" {
		return func() {}, nil
	}
	return holdEnv(filepath.Dir(c.Dir), false)
}

// Describe returns where the commands come from, in words, such as "the managed runtime
// (ansible-core 2.21.4)".
func (c Commands) Describe() string {
	switch c.Source {
	case SourceManaged:
		if c.Problem != "" {
			return "the managed runtime, which cannot be used: " + c.Problem
		}
		return "the managed runtime (ansible-core " + c.Release + ")"
	case SourceConfigured:
		return "the configured directory " + c.Dir
	case SourceImage:
		return "the run's container image"
	default:
		return "PATH"
	}
}

// Locator decides where the Ansible commands come from each time it is asked, so a runtime
// installed or removed while a server runs is used, or no longer used, from the next run on. A run
// asks once and starts every command it runs from that answer.
type Locator struct {
	// bin is the configured commands directory, absolute, or SystemBin, or empty.
	bin string
	// binProblem says why a configured directory cannot be used, empty when it can.
	binProblem string
	// root is the managed runtime's directory.
	root string
}

// NewLocator returns a Locator for the configured commands directory bin, which may be SystemBin
// or empty, and the managed runtime under root, which may be empty for none. A relative bin is
// made absolute here, against this process's working directory, so it names one directory for
// the whole process rather than one inside each run's project checkout.
func NewLocator(bin, root string) *Locator {
	l := &Locator{bin: strings.TrimSpace(bin), root: root}
	if l.bin != "" && l.bin != SystemBin && !filepath.IsAbs(l.bin) {
		abs, err := filepath.Abs(l.bin)
		if err != nil {
			l.binProblem = "the configured directory " + l.bin + " cannot be made absolute: " +
				err.Error()
		}
		l.bin = abs
	}
	return l
}

// Root returns the managed runtime's directory.
func (l *Locator) Root() string {
	if l == nil {
		return ""
	}
	return l.root
}

// Locate returns the commands to run, in this order of preference: the configured directory, then
// the managed runtime when one is in use, then PATH. Neither falls through to the next: a
// configured directory is used even when it holds no Ansible, and a managed runtime in use that is
// broken, or that another account can write, comes back with its Problem set, so a mistake fails
// the run rather than running another Ansible nobody chose. Only a runtime directory with no
// runtime in use means PATH.
func (l *Locator) Locate() Commands {
	if l == nil {
		return Commands{Source: SourcePath}
	}
	switch l.bin {
	case "":
	case SystemBin:
		return Commands{Source: SourcePath, Root: l.root}
	default:
		return Commands{Dir: l.bin, Source: SourceConfigured, Root: l.root, Problem: l.binProblem}
	}
	if l.root == "" {
		return Commands{Source: SourcePath}
	}
	rt, err := Current(l.root)
	switch {
	case err != nil:
		return Commands{Source: SourceManaged, Root: l.root, Problem: err.Error()}
	case rt == nil:
		return Commands{Source: SourcePath, Root: l.root}
	default:
		return Commands{Dir: rt.BinDir(), Source: SourceManaged, Release: rt.Release, Root: l.root}
	}
}

// Runtime is one installed managed runtime.
type Runtime struct {
	// Release is the ansible-core release it holds.
	Release string `json:"ansible_core"`
	// Python is the version of the Python it was built with.
	Python string `json:"python"`
	// LockSHA256 is the hex SHA-256 of the lock it was installed from.
	LockSHA256 string `json:"lock_sha256"`
	// InstalledAt is when the install finished and was verified.
	InstalledAt time.Time `json:"installed_at"`
	// Dir is the environment's directory. The marker lives inside it, so it is not stored there.
	Dir string `json:"dir"`
	// Current reports whether it is the runtime in use.
	Current bool `json:"current"`
}

// BinDir returns the directory holding the runtime's commands.
func (r *Runtime) BinDir() string {
	return filepath.Join(r.Dir, "bin")
}

// envPattern matches an environment's directory name: the release, then the start of the SHA-256
// of the lock it was installed from, then, for an environment built while one of that name already
// existed, a random suffix. Every install builds into a directory nothing else uses, so the
// environment in use is never rebuilt or removed under the runs using it.
var envPattern = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)-([0-9a-f]{12})(?:-[0-9a-f]{8})?$`)

// envName returns the directory name of the environment lock installs into.
func envName(lock *Lock) string {
	return lock.Release + "-" + lock.SHA256()[:digestLen]
}

// DataDirRoot returns the managed runtime's directory: flagValue when set, then the RootEnv
// variable, then DirName under dataDir.
func DataDirRoot(flagValue, dataDir string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(RootEnv)); v != "" {
		return v
	}
	return filepath.Join(dataDir, DirName)
}

// Current returns the runtime in use under root, or nil when none is in use. A current file that
// names a runtime which is missing, unfinished, or one another account owns or can write, is an
// error, which a run treats as a runtime it cannot use rather than as no runtime.
func Current(root string) (*Runtime, error) {
	path := filepath.Join(root, currentName)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err := checkChain(root); err != nil {
		return nil, err
	}
	if err := checkFile(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(string(b))
	if !envPattern.MatchString(name) {
		return nil, fmt.Errorf("%w: %s names %q", ErrNotInstalled,
			filepath.Join(root, currentName), name)
	}
	rt, err := readRuntime(filepath.Join(root, name))
	if err != nil {
		return nil, err
	}
	rt.Current = true
	return rt, nil
}

// readRuntime reads the finished runtime in dir: its marker, which must name the release and lock
// the directory is named for, and the commands a run starts. Nothing in it is believed, or ever
// executed, unless every file and directory in it belongs to this account or root and no other
// account can write it.
func readRuntime(dir string) (*Runtime, error) {
	m := envPattern.FindStringSubmatch(filepath.Base(dir))
	if m == nil {
		return nil, fmt.Errorf("%w: %s is not a runtime environment's name", ErrNotInstalled, dir)
	}
	if _, err := os.Lstat(filepath.Join(dir, markerName)); err != nil {
		return nil, fmt.Errorf("%w: %s has no finished install: %w", ErrNotInstalled, dir, err)
	}
	if err := checkTree(dir); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, markerName))
	if err != nil {
		return nil, fmt.Errorf("%w: %s has no finished install: %w", ErrNotInstalled, dir, err)
	}
	var rt Runtime
	if err := json.Unmarshal(b, &rt); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNotInstalled, filepath.Join(dir, markerName), err)
	}
	if rt.Release != m[1] || !strings.HasPrefix(rt.LockSHA256, m[2]) {
		return nil, fmt.Errorf("%w: %s records ansible-core %q from lock %q", ErrNotInstalled, dir,
			rt.Release, rt.LockSHA256)
	}
	rt.Dir = dir
	for _, name := range []string{"ansible", "ansible-playbook", "ansible-inventory"} {
		if !isExecutable(filepath.Join(rt.BinDir(), name)) {
			return nil, fmt.Errorf("%w: %s has no %s", ErrNotInstalled, dir, name)
		}
	}
	return &rt, nil
}

// List returns the finished runtimes under root, newest release first and, within a release, the
// most recently installed first, with the one in use marked.
func List(root string) ([]*Runtime, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cur, _ := Current(root)
	var out []*Runtime
	for _, e := range entries {
		if !e.IsDir() || !envPattern.MatchString(e.Name()) {
			continue
		}
		rt, err := readRuntime(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		rt.Current = cur != nil && cur.Dir == rt.Dir
		out = append(out, rt)
	}
	slices.SortFunc(out, func(a, b *Runtime) int {
		if c := compareReleases(b.Release, a.Release); c != 0 {
			return c
		}
		return b.InstalledAt.Compare(a.InstalledAt)
	})
	return out, nil
}

// Remove deletes every environment of the release version names under root, or every environment
// when version is empty, and returns the directories it removed. It refuses, removing nothing,
// when a run is using one of them. Removing the runtime in use moves current to the newest one
// left, or clears it. Only directories the installer made are removed.
func Remove(root, version string) ([]string, error) {
	want, err := removeTarget(version)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		if version != "" {
			return nil, fmt.Errorf("%w: no ansible-core %s under %s", ErrNotInstalled, version, root)
		}
		return nil, nil
	}
	unlock, err := lockRoot(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, e := range entries {
		m := envPattern.FindStringSubmatch(e.Name())
		if e.IsDir() && m != nil && want(m[1]) {
			targets = append(targets, filepath.Join(root, e.Name()))
		}
	}
	if version != "" && len(targets) == 0 {
		return nil, fmt.Errorf("%w: no ansible-core %s under %s", ErrNotInstalled, version, root)
	}
	// Every environment is held exclusively before any is deleted, so a run using one stops the
	// whole remove, and no run can start on one between the check and the delete.
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for _, dir := range targets {
		if _, err := os.Lstat(filepath.Join(dir, markerName)); err != nil {
			continue
		}
		release, err := holdEnv(dir, true)
		if err != nil {
			return nil, fmt.Errorf("%w. Remove it once the runs using it finish", err)
		}
		releases = append(releases, release)
	}
	var removed []string
	for _, dir := range targets {
		if err := removeEnv(dir); err != nil {
			return removed, err
		}
		removed = append(removed, dir)
	}
	if err := repointCurrent(root); err != nil {
		return removed, err
	}
	if version == "" {
		// The root is removed only when nothing but the installer's own files was in it.
		_ = os.Remove(filepath.Join(root, ".lock"))
		_ = os.Remove(root)
	}
	return removed, nil
}

// removeTarget returns the test that picks which release directories a remove deletes.
func removeTarget(version string) (func(string) bool, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	switch {
	case version == "":
		return func(string) bool { return true }, nil
	case releasePattern.MatchString(version):
		return func(name string) bool { return name == version }, nil
	case minorPattern.MatchString(version):
		return func(name string) bool { return minorOf(name) == version }, nil
	default:
		return nil, fmt.Errorf("%w: %q is not a release such as 2.21.4 or a minor version such as "+
			"2.21", ErrVersion, version)
	}
}

// repointCurrent makes current name the newest finished runtime left under root, or removes it
// when none is left.
func repointCurrent(root string) error {
	if cur, err := Current(root); err == nil && cur != nil {
		return nil
	}
	left, err := List(root)
	if err != nil {
		return err
	}
	if len(left) == 0 {
		err := os.Remove(filepath.Join(root, currentName))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return setCurrent(root, filepath.Base(left[0].Dir))
}

// removeEnv deletes the environment directory dir when the installer made it, and refuses to touch
// one it did not.
func removeEnv(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !fileExists(filepath.Join(dir, markerName)) && !fileExists(filepath.Join(dir, "pyvenv.cfg")) {
		return fmt.Errorf("%w: %s is in the runtime's place and was not made by the installer, so "+
			"it is left alone. Move it, or name another directory with --dir", ErrNotManaged, dir)
	}
	return os.RemoveAll(dir)
}

// setCurrent atomically makes the environment named name the runtime in use under root.
func setCurrent(root, name string) error {
	return writeFileAtomic(filepath.Join(root, currentName), []byte(name+"\n"))
}

// writeFileAtomic writes data to path through a temporary file renamed over it, so a reader sees
// the old content or the new, never part of either.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// fileExists reports whether path names a regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// isExecutable reports whether path names a regular file someone may execute.
func isExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}
