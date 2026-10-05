package runfiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source says where a run-files root came from.
type Source string

const (
	// SourceFlag is a root an operator named with --runfiles-dir.
	SourceFlag Source = "flag"
	// SourceSystemd is the runtime directory systemd made for the service, under /run, which is
	// memory-backed and removed by systemd when the service stops for any reason.
	SourceSystemd Source = "systemd"
	// SourceXDG is the account's XDG runtime directory, a per-user tmpfs, used only when it is owned
	// by this account and closed to everyone else.
	SourceXDG Source = "xdg"
	// SourceTemp is the system temporary directory, the fallback the doctor warns about.
	SourceTemp Source = "temp"
)

const (
	// envRuntimeDirectory is the variable systemd sets to the paths a unit's RuntimeDirectory names.
	envRuntimeDirectory = "RUNTIME_DIRECTORY"
	// envXDGRuntimeDir is the variable naming the account's XDG runtime directory.
	envXDGRuntimeDir = "XDG_RUNTIME_DIR"
	// systemdSubdir is the directory the root takes inside a systemd runtime directory, so the
	// service can keep other things beside it.
	systemdSubdir = "runfiles"
	// xdgSubdir is the directory the root takes inside an XDG runtime directory, which the account
	// shares with every other program it runs.
	xdgSubdir = "switchtender-runfiles"
	// devShm is the shared memory mount, never used as a root.
	devShm = "/dev/shm"
)

// Choice is the run-files root a process will use and where it came from.
type Choice struct {
	// Path is the root's absolute path.
	Path string
	// Source says which rule picked it.
	Source Source
	// Passed lists each preferred location that was passed over and why, for the startup log and the
	// doctor.
	Passed []string
}

// ChooseRoot picks the run-files root. An explicit root wins and must be absolute. Without one it
// takes, in order, the systemd runtime directory when the process runs as a unit that sets one, the
// account's XDG runtime directory when it is owned by this account and private, and otherwise the
// temporary directory. It never picks /dev/shm, which every process on the host shares. getenv
// reads the environment, and nil uses the process environment.
//
// ChooseRoot only picks. Prepare proves the choice is safe before anything is staged in it.
func ChooseRoot(explicit string, getenv func(string) string) (Choice, error) {
	if explicit != "" {
		if !filepath.IsAbs(explicit) {
			return Choice{}, fmt.Errorf("%w: --runfiles-dir %q is not an absolute path", ErrUnsafeRoot,
				explicit)
		}
		return Choice{Path: filepath.Clean(explicit), Source: SourceFlag}, nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	var passed []string
	if dirs := getenv(envRuntimeDirectory); dirs != "" {
		// A unit naming several runtime directories gets them colon separated, in the order named.
		first, _, _ := strings.Cut(dirs, ":")
		if filepath.IsAbs(first) {
			return Choice{Path: filepath.Join(first, systemdSubdir), Source: SourceSystemd}, nil
		}
		passed = append(passed, envRuntimeDirectory+" "+first+" is not an absolute path")
	}
	if xdg := getenv(envXDGRuntimeDir); xdg != "" {
		if err := privateDir(xdg); err != nil {
			passed = append(passed, envXDGRuntimeDir+" "+xdg+" "+err.Error())
		} else {
			return Choice{Path: filepath.Join(xdg, xdgSubdir), Source: SourceXDG, Passed: passed}, nil
		}
	}
	return Choice{Path: DefaultRoot(), Source: SourceTemp, Passed: passed}, nil
}

// underDevShm reports whether path is /dev/shm or inside it.
func underDevShm(path string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	return path == devShm || strings.HasPrefix(path, devShm+"/")
}
