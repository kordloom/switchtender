package coredump

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const (
	// hideChildEnv turns a re-executed copy of this test binary into a helper. Its value names what
	// the helper does.
	hideChildEnv = "SWITCHTENDER_HIDE_ENVIRON_CHILD"
	// keyMarker is the encryption key the helper is started with, the value a run must not read.
	keyMarker = "marker-123"
	// toolRan is what the helper prints when it runs as a tool started from /proc/self/exe.
	toolRan = "tool ran"
)

// init runs the helper when this test binary was started as one. It runs before TestMain, which
// belongs to the core limit tests and knows nothing of these modes.
func init() {
	mode := os.Getenv(hideChildEnv)
	if mode == "" {
		return
	}
	out, err := hideHelper(mode)
	if err != nil {
		fmt.Println("ERR " + err.Error())
		os.Exit(1)
	}
	fmt.Print(out)
	os.Exit(0)
}

// hideHelper does what mode names. "hide" and "keep" start a shell that reads this process's
// environ the way a host run would, after calling HideEnviron or not, and describe the result.
// "tool" calls HideEnviron and starts this binary again through /proc/self/exe, which is how the
// tool shim starts every tool, and reports what it printed. "echo" is that tool.
func hideHelper(mode string) (string, error) {
	switch mode {
	case "echo":
		return toolRan + "\n", nil
	case "hide", "tool":
		if err := HideEnviron(); err != nil {
			return "", err
		}
	case "keep":
	default:
		return "", fmt.Errorf("unknown mode %q", mode)
	}
	if mode == "tool" {
		cmd := exec.Command("/proc/self/exe", "-test.run=^$")
		cmd.Env = append(os.Environ(), hideChildEnv+"=echo")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("start a tool from /proc/self/exe: %w: %s", err, out)
		}
		return strings.TrimSpace(string(out)) + "\n", nil
	}
	cmd := exec.Command("sh", "-c", `cat "/proc/$PPID/environ"`)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", fmt.Errorf("start the reader: %w", err)
	}
	return fmt.Sprintf("read=%t marker=%t output=%q\n", err == nil,
		strings.Contains(string(out), keyMarker), out), nil
}

// TestHideEnvironKeepsTheKeyFromARun proves that once the process calls HideEnviron, a child of the
// same account cannot read the process's environ, and the key it was started with stays out of the
// child's hands. The same helper without the call hands the child the key, which is the negative
// control. Root reads any process's entries regardless, so the test cannot show anything as root.
func TestHideEnvironKeepsTheKeyFromARun(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and the test runs as root, which reads " +
				"any process's environ, so the non-dumpable flag cannot be shown")
		}
		t.Skip("running as root, which reads any process's environ regardless of the flag")
	}
	tests := []struct {
		// Mode says whether the helper calls HideEnviron before the read.
		Mode string
		// WantRead and WantMarker say whether the read succeeds and shows the key.
		WantRead   bool
		WantMarker bool
	}{{ // Test 0: Hidden: the read is refused and the key is absent.
		Mode: "hide", WantRead: false, WantMarker: false,
	}, { // Test 1: Not hidden: the same read returns the key.
		Mode: "keep", WantRead: true, WantMarker: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Mode), func(t *testing.T) {
			t.Parallel()
			line := runHideHelper(t, test.Mode)
			var read, marker bool
			var output string
			_, err := fmt.Sscanf(line, "read=%t marker=%t output=%q", &read, &marker, &output)
			if err != nil {
				t.Fatalf("helper reported %q: %v", line, err)
			}
			if diff := cmp.Diff([]bool{test.WantRead, test.WantMarker}, []bool{read, marker}); diff != "" {
				t.Errorf("helper %s read and marker (-want +got):\n%s\nreader output: %q", test.Mode,
					diff, output)
			}
		})
	}
}

// TestHideEnvironStillStartsTools proves a process marked non-dumpable still starts a tool through
// /proc/self/exe, which is how the tool shim starts every tool on Linux, and that the tool runs.
func TestHideEnvironStillStartsTools(t *testing.T) {
	t.Parallel()
	if got := runHideHelper(t, "tool"); got != toolRan {
		t.Errorf("helper tool printed %q, want %q", got, toolRan)
	}
}

// runHideHelper starts this test binary as a helper in mode, with the key marker in its first
// environment, and returns the line it printed.
func runHideHelper(t *testing.T, mode string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), hideChildEnv+"="+mode, "SWITCHTENDER_ENCRYPTION_KEY="+keyMarker)
	out, err := cmd.Output()
	line := strings.TrimSpace(string(out))
	if err != nil || strings.HasPrefix(line, "ERR") {
		t.Fatalf("helper %s: %q, %v", mode, line, err)
	}
	return line
}
