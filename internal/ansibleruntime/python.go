package ansibleruntime

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// pythonProbe prints the interpreter's version as major.minor.micro.
	pythonProbe = "import sys; print('%d.%d.%d' % sys.version_info[:3])"
	// venvProbe imports what building a virtual environment with pip in it needs.
	venvProbe = "import venv, ensurepip"
	// probeTimeout bounds one interpreter probe.
	probeTimeout = 30 * time.Second
)

// pythonVersionPattern matches the version pythonProbe prints.
var pythonVersionPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// python is an interpreter an install builds its environment with.
type python struct {
	// Path is the interpreter's path.
	Path string
	// Version is its version, such as 3.12.3.
	Version string
}

// pythonCandidates returns the interpreters tried, in order, when none is named: python3, then each
// python3.N in the lock's range, newest first.
func pythonCandidates(lock *Lock) []string {
	out := []string{"python3"}
	for n := pythonMinor(lock.PythonMax); n >= pythonMinor(lock.PythonMin) && n >= 0; n-- {
		out = append(out, fmt.Sprintf("python3.%d", n))
	}
	return out
}

// findPython returns an interpreter the lock's release runs on: the one named, when one is, and
// otherwise the first candidate on PATH inside the lock's Python range. The error states the range
// the release needs and every interpreter that was found outside it.
func findPython(ctx context.Context, named string, lock *Lock) (*python, error) {
	need := fmt.Sprintf("ansible-core %s runs on Python %s to %s", lock.Release, lock.PythonMin,
		lock.PythonMax)
	candidates := pythonCandidates(lock)
	if named != "" {
		candidates = []string{named}
	}
	var found []string
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			if named != "" {
				return nil, fmt.Errorf("%w: %s, and %s was not found", ErrPython, need, named)
			}
			continue
		}
		// The interpreter is resolved once, so the one probed is the one the environment is built
		// with, whatever a symlink or an alternatives switch points at later.
		if real, err := filepath.EvalSymlinks(path); err == nil {
			if abs, err := filepath.Abs(real); err == nil {
				path = abs
			}
		}
		version, err := probe(ctx, path, pythonProbe)
		if err != nil {
			found = append(found, fmt.Sprintf("%s did not report its version (%v)", path, err))
			continue
		}
		if !pythonInRange(version, lock) {
			found = append(found, fmt.Sprintf("%s is Python %s", path, version))
			continue
		}
		if _, err := probe(ctx, path, venvProbe); err != nil {
			return nil, fmt.Errorf("%w: %s at %s cannot build a virtual environment, because its "+
				"venv or ensurepip module is missing. On Debian and Ubuntu, install the "+
				"python%s-venv package", ErrPython, "Python "+version, path, minorOf(version))
		}
		return &python{Path: path, Version: version}, nil
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: %s, and no python3 was found on PATH. Install Python %s to %s, "+
			"or name an interpreter with --python", ErrPython, need, lock.PythonMin, lock.PythonMax)
	}
	return nil, fmt.Errorf("%w: %s, and none was found: %s. Install Python %s to %s, or name an "+
		"interpreter with --python", ErrPython, need, strings.Join(found, ", "), lock.PythonMin,
		lock.PythonMax)
}

// probe runs the interpreter at path, isolated and in the install's environment, with code and
// returns what it printed, trimmed.
func probe(ctx context.Context, path, code string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-I", "-c", code)
	cmd.Env = installEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// pythonInRange reports whether version, as major.minor.micro, is a Python 3 inside the lock's
// range.
func pythonInRange(version string, lock *Lock) bool {
	m := pythonVersionPattern.FindStringSubmatch(version)
	if m == nil || m[1] != "3" {
		return false
	}
	n := pythonMinor("3." + m[2])
	return n >= pythonMinor(lock.PythonMin) && n <= pythonMinor(lock.PythonMax)
}
