//go:build !windows

package ansibleruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePythonOpts shapes how the fake interpreter behaves.
type fakePythonOpts struct {
	// Version is what the interpreter reports as its version.
	Version string
	// NoVenv makes the venv and ensurepip import fail.
	NoVenv bool
	// HashMismatch makes pip fail the way it does when a file does not match the lock.
	HashMismatch bool
	// Reports overrides the release the installed ansible --version reports.
	Reports string
}

// fakePythonScript is a POSIX shell stand-in for python3: it answers the version and venv probes,
// makes a virtual environment that is a copy of itself, and, as that environment's pip, logs its
// arguments and writes ansible commands that report the release the lock pins.
const fakePythonScript = `#!/bin/sh
log='__LOG__'
while :; do
  case "$1" in
  -I|-E|-s|-B|-P) shift ;;
  *) break ;;
  esac
done
case "$1" in
-c)
  case "$2" in
  *version_info*) echo '__VERSION__'; exit 0 ;;
  *ensurepip*) __VENV__ ;;
  esac
  exit 1 ;;
-m)
  case "$2" in
  venv)
    mkdir -p "$3/bin" || exit 1
    printf 'home = fake\n' > "$3/pyvenv.cfg"
    cp "$0" "$3/bin/python" && chmod 755 "$3/bin/python"
    echo "venv $3" >> "$log"
    exit 0 ;;
  pip)
    echo "pip $*" >> "$log"
    lock=''
    prev=''
    for a in "$@"; do
      if [ "$prev" = "-r" ]; then lock="$a"; fi
      prev="$a"
    done
    if [ '__MISMATCH__' = 1 ]; then
      echo 'ERROR: THESE PACKAGES DO NOT MATCH THE HASHES FROM THE REQUIREMENTS FILE.'
      exit 1
    fi
    release=$(sed -n 's/^ansible-core==\([0-9.]*\).*/\1/p' "$lock")
    if [ -n '__REPORTS__' ]; then release='__REPORTS__'; fi
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

// fakePython writes the fake interpreter into a temporary directory and returns its path and the
// path of the log its venv and pip calls append to.
func fakePython(t *testing.T, opts fakePythonOpts) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	venv := "exit 0"
	if opts.NoVenv {
		venv = "exit 1"
	}
	mismatch := "0"
	if opts.HashMismatch {
		mismatch = "1"
	}
	script := strings.NewReplacer("__LOG__", logPath, "__VERSION__", opts.Version,
		"__VENV__", venv, "__MISMATCH__", mismatch, "__REPORTS__", opts.Reports).
		Replace(fakePythonScript)
	path := filepath.Join(dir, "python3")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake python: %v", err)
	}
	return path, logPath
}

// calls returns the lines of the fake interpreter's log, nil when it logged nothing.
func calls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the fake python log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// countPrefix returns how many lines start with prefix.
func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// fakeLockSHA256 is the lock digest fakeRuntime records.
var fakeLockSHA256 = strings.Repeat("0", 64)

// fakeRuntime lays out a finished managed runtime for release under root without any Python, for
// the tests that only locate one, and makes it current when current is set.
func fakeRuntime(t *testing.T, root, release string, current bool) string {
	t.Helper()
	name := release + "-" + fakeLockSHA256[:digestLen]
	dir := filepath.Join(root, name)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, c := range []string{"ansible", "ansible-playbook", "ansible-inventory"} {
		if err := os.WriteFile(filepath.Join(bin, c), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", c, err)
		}
	}
	m := `{"ansible_core":"` + release + `","python":"3.12.3","lock_sha256":"` + fakeLockSHA256 + `"}`
	if err := os.WriteFile(filepath.Join(dir, markerName), []byte(m), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if current {
		if err := setCurrent(root, name); err != nil {
			t.Fatalf("setCurrent: %v", err)
		}
	}
	return dir
}
