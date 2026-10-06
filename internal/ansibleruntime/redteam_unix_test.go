//go:build !windows

package ansibleruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envFakePythonScript is a POSIX shell stand-in for python3 that records, for its venv and pip
// calls, whether it was started isolated (-I or -E) and the variables that change what pip installs
// or which code the interpreter imports. As pip it fails the way pip does when its --find-links
// directory does not exist from where pip runs, and otherwise writes ansible commands that report
// the release the lock pins.
const envFakePythonScript = `#!/bin/sh
log='__LOG__'
iso=0
while :; do
  case "$1" in
  -I|-E) iso=1; shift ;;
  -s|-B|-P) shift ;;
  *) break ;;
  esac
done
envline() {
  printf 'PYTHONPATH=%s|PYTHONHOME=%s|PIP_REQUIREMENT=%s|PIP_CONFIG_FILE=%s' "${PYTHONPATH-}" \
    "${PYTHONHOME-}" "${PIP_REQUIREMENT-}" "${PIP_CONFIG_FILE-}"
}
case "$1" in
-c)
  case "$2" in
  *version_info*) echo '__VERSION__'; exit 0 ;;
  *ensurepip*) exit 0 ;;
  esac
  exit 1 ;;
-m)
  case "$2" in
  venv)
    mkdir -p "$3/bin" || exit 1
    printf 'home = fake\n' > "$3/pyvenv.cfg"
    cp "$0" "$3/bin/python" && chmod 755 "$3/bin/python"
    echo "venv iso=$iso $(envline)" >> "$log"
    exit 0 ;;
  pip)
    echo "pip iso=$iso $(envline) args=$*" >> "$log"
    lock=''
    links=''
    prev=''
    for a in "$@"; do
      if [ "$prev" = "-r" ]; then lock="$a"; fi
      if [ "$prev" = "--find-links" ]; then links="$a"; fi
      prev="$a"
    done
    if [ -n "$links" ] && [ ! -d "$links" ]; then
      echo "WARNING: Location '$links' is ignored: it is either a non-existing path or" \
        "lacks a specific scheme."
      echo "ERROR: No matching distribution found for ansible-core"
      exit 1
    fi
    release=$(sed -n 's/^ansible-core==\([0-9.]*\).*/\1/p' "$lock")
    bin=$(dirname "$0")
    for c in ansible ansible-playbook ansible-inventory ansible-galaxy; do
      printf '#!/bin/sh\necho "%s [core %s]"\n' "$c" "$release" > "$bin/$c"
      chmod 755 "$bin/$c"
    done
    exit 0 ;;
  esac ;;
esac
exit 2
`

// envFakePython writes envFakePythonScript into a temporary directory and returns its path and the
// path of the log its venv and pip calls append to.
func envFakePython(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := strings.NewReplacer("__LOG__", logPath, "__VERSION__", "3.12.3").
		Replace(envFakePythonScript)
	path := filepath.Join(dir, "python3")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake python: %v", err)
	}
	return path, logPath
}

// logField returns the value of the field name in a line envFakePythonScript logged.
func logField(line, name string) string {
	for _, part := range strings.Split(line, "|") {
		for _, f := range strings.Fields(part) {
			if v, ok := strings.CutPrefix(f, name+"="); ok {
				return v
			}
		}
	}
	return ""
}

// TestLocateRefusesARuntimeOtherAccountsCanWrite pins that the server never starts Ansible from a
// managed runtime another local account could have planted or changed. Today Locate trusts any
// directory whose marker and commands look right, whoever owns it and whoever can write to it, so
// a runtime directory under /tmp, a shared data directory, or SWITCHTENDER_ANSIBLE_RUNTIME_DIR
// pointed at a writable path hands every run to whoever wrote there.
func TestLocateRefusesARuntimeOtherAccountsCanWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Chmod func(root, env string) error
	}{{ // Test 0: A world-writable runtime directory, where anyone can plant current and a runtime.
		Chmod: func(root, _ string) error { return os.Chmod(root, 0o777) },
	}, { // Test 1: A group-writable environment, whose commands any group member can replace.
		Chmod: func(_, env string) error { return os.Chmod(env, 0o775) },
	}, { // Test 2: A world-writable ansible-playbook.
		Chmod: func(_, env string) error {
			return os.Chmod(filepath.Join(env, "bin", "ansible-playbook"), 0o777)
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "ansible")
			env := fakeRuntime(t, root, "2.21.4", true)
			if err := test.Chmod(root, env); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			got := NewLocator("", root).Locate()
			if got.Dir == filepath.Join(env, "bin") {
				t.Errorf("Locate() = %+v: it starts Ansible from a runtime another account can write",
					got)
			}
		})
	}
}

// TestInstallRefusesARootOtherAccountsCanWrite pins that an install does not build into a runtime
// directory another account can write, where it could plant the environment, its marker, or
// current around the install.
func TestInstallRefusesARootOtherAccountsCanWrite(t *testing.T) {
	t.Parallel()
	py, logPath := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(root, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := Install(context.Background(), InstallOptions{Root: root, Version: "2.21", Python: py})
	if err == nil {
		t.Errorf("Install() into a world-writable runtime directory succeeded, with calls %q",
			calls(t, logPath))
	}
}

// TestTheRootLockDoesNotFollowAPlantedSymlink pins that install and remove never create a file
// through a .lock symlink someone planted in the runtime directory. The lock is opened with
// O_CREATE and no O_NOFOLLOW, so an install run as root into a directory another account controls
// creates any file the symlink names, /etc/nologin among them.
func TestTheRootLockDoesNotFollowAPlantedSymlink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Install bool
	}{{  // Test 0: Remove.
	}, { // Test 1: Install.
		Install: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			root := filepath.Join(base, "ansible")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			target := filepath.Join(base, "elsewhere", "created-through-the-symlink")
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Symlink(target, filepath.Join(root, ".lock")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			if test.Install {
				py, _ := fakePython(t, fakePythonOpts{Version: "3.12.3"})
				_, _ = Install(context.Background(), InstallOptions{Root: root, Version: "2.21",
					Python: py})
			} else {
				_, _ = Remove(root, "")
			}
			if _, err := os.Lstat(target); err == nil {
				t.Errorf("the planted .lock symlink made the installer create %s", target)
			}
		})
	}
}

// TestInstallIsolatesPythonAndPipFromTheCallersEnvironment pins that the hash-checked install
// cannot be steered by the environment it is started from. Today venv and pip inherit everything:
// PYTHONPATH and PYTHONHOME replace the code the interpreter runs, pip included, and
// PIP_REQUIREMENT or a requirement line in the file PIP_CONFIG_FILE names adds packages of the
// caller's choosing, each with its own hash, to the install. pip 26.2.1 was shown to read both.
//
// It sets the process environment, so it does not run in parallel.
func TestInstallIsolatesPythonAndPipFromTheCallersEnvironment(t *testing.T) {
	planted := t.TempDir()
	conf := filepath.Join(planted, "pip.conf")
	extra := filepath.Join(planted, "extra.txt")
	if err := os.WriteFile(extra, []byte("evilpkg==6.6.6 --hash=sha256:"+strings.Repeat("b", 64)+
		"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(conf, []byte("[global]\nrequirement = "+extra+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PYTHONPATH", planted)
	t.Setenv("PYTHONHOME", planted)
	t.Setenv("PIP_REQUIREMENT", extra)
	t.Setenv("PIP_CONFIG_FILE", conf)
	py, logPath := envFakePython(t)
	root := filepath.Join(t.TempDir(), "ansible")
	if _, err := Install(context.Background(), InstallOptions{Root: root, Version: "2.21",
		Python: py}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	lines := calls(t, logPath)
	if len(lines) == 0 {
		t.Fatal("the fake python logged no venv or pip call")
	}
	for _, line := range lines {
		isolated := logField(line, "iso") == "1"
		if !isolated && (logField(line, "PYTHONPATH") != "" || logField(line, "PYTHONHOME") != "") {
			t.Errorf("%s ran with the caller's PYTHONPATH or PYTHONHOME and not isolated: %s",
				firstField(line), line)
		}
		if !strings.HasPrefix(line, "pip ") {
			continue
		}
		if logField(line, "PIP_REQUIREMENT") != "" {
			t.Errorf("pip ran with the caller's PIP_REQUIREMENT, which adds packages: %s", line)
		}
		if logField(line, "PIP_CONFIG_FILE") != os.DevNull {
			t.Errorf("pip read configuration files, which can add packages: PIP_CONFIG_FILE=%q",
				logField(line, "PIP_CONFIG_FILE"))
		}
	}
}

// TestAFailedReinstallKeepsTheRuntimeInUse pins the documented promise that a failed install
// leaves the release in use in use. Today an install of the release in use that finds it does not
// verify, for example because ansible --version timed out under load or the installing shell set
// PYTHONHOME, deletes that environment before its replacement exists. When the replacement then
// fails too, current names nothing and every run falls back to whatever is on PATH.
func TestAFailedReinstallKeepsTheRuntimeInUse(t *testing.T) {
	t.Parallel()
	py, _ := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	root := filepath.Join(t.TempDir(), "ansible")
	res, err := Install(context.Background(), InstallOptions{Root: root, Version: "2.21", Python: py})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	env := res.Runtime.Dir
	flag := filepath.Join(t.TempDir(), "transient")
	transient := "#!/bin/sh\nif [ -e '" + flag + "' ]; then exit 1; fi\necho 'ansible [core " +
		res.Runtime.Release + "]'\n"
	ansible := filepath.Join(env, "bin", "ansible")
	if err := os.WriteFile(ansible, []byte(transient), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	broken, _ := fakePython(t, fakePythonOpts{Version: "3.12.3", HashMismatch: true})
	if _, err := Install(context.Background(), InstallOptions{Root: root, Version: "2.21",
		Python: broken}); err == nil {
		t.Fatal("the second Install() succeeded with a pip that fails")
	}
	if err := os.Remove(flag); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got := NewLocator("", root).Locate()
	if got.Source != SourceManaged || got.Dir != filepath.Join(env, "bin") {
		t.Errorf("after a failed reinstall Locate() = %+v, want the runtime that was in use, %s",
			got, env)
	}
}

// TestLocateFailsClosedWhenTheRuntimeInUseIsBroken pins that a managed runtime named in use but
// unusable fails the run, the way a configured directory without Ansible does, rather than quietly
// running the Ansible first on PATH. Today Locate treats it as no runtime, so during every
// reinstall, and after a failed one, runs start whatever ansible-playbook PATH finds first.
func TestLocateFailsClosedWhenTheRuntimeInUseIsBroken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Break      func(t *testing.T, root, env string)
		WantSource string
	}{{ // Test 0: The environment in use has lost its marker, as during a reinstall.
		Break: func(t *testing.T, _, env string) {
			if err := os.Remove(filepath.Join(env, markerName)); err != nil {
				t.Fatalf("remove: %v", err)
			}
		},
	}, { // Test 1: The environment in use is gone, as after a failed reinstall.
		Break: func(t *testing.T, _, env string) {
			if err := os.RemoveAll(env); err != nil {
				t.Fatalf("remove: %v", err)
			}
		},
	}, { // Test 2: With no runtime installed at all, PATH is right.
		Break: func(t *testing.T, root, env string) {
			if err := os.RemoveAll(env); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if err := os.Remove(filepath.Join(root, currentName)); err != nil {
				t.Fatalf("remove: %v", err)
			}
		},
		WantSource: SourcePath,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "ansible")
			env := fakeRuntime(t, root, "2.21.4", true)
			test.Break(t, root, env)
			got := NewLocator("", root).Locate()
			switch {
			case test.WantSource != "" && got.Source != test.WantSource:
				t.Errorf("Locate() = %+v, want source %q", got, test.WantSource)
			case test.WantSource == "" && got.Source == SourcePath:
				t.Errorf("Locate() = %+v: a broken runtime in use falls through to PATH", got)
			}
		})
	}
}

// TestInstallFindsARelativeWheelDirectory pins that --wheels names a directory relative to where
// the command runs, as the documented `switchtender ansible install --wheels ./wheels` does. Today
// the path reaches pip unchanged and pip runs in the new environment's directory, where it does
// not exist, so pip ignores it and the offline install fails. pip 26.2.1 was shown to do exactly
// that.
func TestInstallFindsARelativeWheelDirectory(t *testing.T) {
	t.Parallel()
	py, logPath := envFakePython(t)
	root := filepath.Join(t.TempDir(), "ansible")
	// locks is a directory relative to the test's working directory, the package directory.
	_, err := Install(context.Background(), InstallOptions{Root: root, Version: "2.21", Python: py,
		Wheels: "locks"})
	if err != nil {
		t.Errorf("Install() with a relative wheel directory error = %v", err)
	}
	for _, line := range calls(t, logPath) {
		if !strings.HasPrefix(line, "pip ") {
			continue
		}
		if _, links, ok := strings.Cut(line, "--find-links "); ok &&
			!filepath.IsAbs(firstField(links)) {
			t.Errorf("pip was handed the relative wheel directory %q", firstField(links))
		}
	}
}

// TestInstallDoesNotRunCodeOtherAccountsCanWrite pins that an install never executes an existing
// environment's commands when another account can change them. Install re-verifies a finished
// environment by running its ansible --version, so `sudo switchtender ansible install` against a
// runtime directory the server's account can write, the default data directory included, runs
// whatever a playbook last wrote there as root. Write permission for others stands in here for a
// directory owned by another account, which a test cannot create without root.
func TestInstallDoesNotRunCodeOtherAccountsCanWrite(t *testing.T) {
	t.Parallel()
	lock, err := LockFor("2.21")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "ansible")
	env := filepath.Join(root, envName(lock))
	bin := filepath.Join(env, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ran := filepath.Join(base, "ran-as-the-installer")
	planted := "#!/bin/sh\n: > '" + ran + "'\necho 'ansible [core " + lock.Release + "]'\n"
	for _, c := range []string{"ansible", "ansible-playbook", "ansible-inventory"} {
		if err := os.WriteFile(filepath.Join(bin, c), []byte(planted), 0o777); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chmod(filepath.Join(bin, c), 0o777); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	}
	m := `{"ansible_core":"` + lock.Release + `","python":"3.12.3","lock_sha256":"` + lock.SHA256() +
		`"}`
	if err := os.WriteFile(filepath.Join(env, markerName), []byte(m), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	py, _ := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	_, _ = Install(context.Background(), InstallOptions{Root: root, Version: "2.21", Python: py})
	if _, err := os.Stat(ran); err == nil {
		t.Error("Install() executed a world-writable ansible from the runtime directory")
	}
}

// TestTheRecordedPythonIsTheEnvironments pins that the Python a runtime records is the one its
// environment runs. Today the installer probes the interpreter by path, then starts that path
// again to build the environment and records the probe's answer, so a symlink or shim that changes
// between the two, such as an alternatives switch or a pyenv shim, builds the environment with an
// interpreter that was never checked against the release's range, and the marker says otherwise.
func TestTheRecordedPythonIsTheEnvironments(t *testing.T) {
	t.Parallel()
	py, _ := fakePython(t, fakePythonOpts{Version: "3.12.3"})
	// The interpreter the environment is built with answers 3.9.18, standing in for the path
	// resolving to another interpreter by the time the environment is made.
	script, err := os.ReadFile(py)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	swapped := strings.Replace(string(script), "*version_info*) echo '3.12.3'",
		"*version_info*) if [ -f \"$(dirname \"$0\")/../pyvenv.cfg\" ]; then echo '3.9.18'; "+
			"else echo '3.12.3'; fi", 1)
	if swapped == string(script) {
		t.Fatal("the fake python's version line was not found")
	}
	if err := os.WriteFile(py, []byte(swapped), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	root := filepath.Join(t.TempDir(), "ansible")
	res, err := Install(context.Background(), InstallOptions{Root: root, Version: "2.21", Python: py})
	if err != nil {
		return
	}
	got, err := probe(context.Background(), filepath.Join(res.Runtime.Dir, "bin", "python"),
		pythonProbe)
	if err != nil {
		t.Fatalf("probe the environment's python: %v", err)
	}
	if got != res.Runtime.Python {
		t.Errorf("the runtime records Python %s, and its environment runs Python %s, outside "+
			"ansible-core %s's range", res.Runtime.Python, got, res.Runtime.Release)
	}
}
