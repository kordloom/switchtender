//go:build unix

package coredump

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// childEnv turns a re-executed copy of this test binary into a process that reports its core limit.
// Its value says whether the child calls Disable first.
const childEnv = "SWITCHTENDER_COREDUMP_CHILD"

// raisedLimit is the soft core limit the child raises itself to before anything else, so a limit
// that reads zero afterward can only be Disable's doing and not the environment's default.
const raisedLimit = 1 << 20

// TestMain lets the test binary double as the process whose limits are measured, since Disable
// changes the process it runs in for good.
func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		out, err := report(mode == "disable")
		if err != nil {
			fmt.Println("ERR " + err.Error())
			os.Exit(1)
		}
		fmt.Print(out)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// report raises the soft core limit, calls Disable when told to, and describes the limit the
// process then has, whether it can raise it again, and the limit a tool it starts inherits.
func report(disable bool) (string, error) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &rl); err != nil {
		return "", err
	}
	if rl.Max != unix.RLIM_INFINITY && rl.Max < raisedLimit {
		return "", fmt.Errorf("the hard core limit is %d, too low to show the change", rl.Max)
	}
	raised := &unix.Rlimit{Cur: raisedLimit, Max: rl.Max}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, raised); err != nil {
		return "", err
	}
	if disable {
		if err := Disable(); err != nil {
			return "", err
		}
	}
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &rl); err != nil {
		return "", err
	}
	raise := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: raisedLimit, Max: raisedLimit})
	tool, err := exec.Command("sh", "-c", "ulimit -c").Output()
	if err != nil {
		return "", err
	}
	// The raise attempt is reported after the tool runs, so a failed raise cannot change what the
	// tool inherits.
	return fmt.Sprintf("soft=%d hard=%d raised=%t tool=%s\n", rl.Cur, rl.Max, raise == nil,
		strings.TrimSpace(string(tool))), nil
}

// TestDisableTurnsOffCoreDumpsForGood proves Disable sets both core limits to zero, that nothing in
// the process can raise them again, and that a tool the process starts inherits the zero. Without
// the call the same child keeps the raised limit and hands it on, which is the negative control.
func TestDisableTurnsOffCoreDumpsForGood(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Mode says whether the child calls Disable.
		Mode string
		// WantSoft, WantRaised, and WantToolZero describe the child's limits afterward. The tool's
		// limit is compared only against zero, since shells count it in blocks of different sizes.
		WantSoft     uint64
		WantRaised   bool
		WantToolZero bool
	}{{ // Test 0: Disabled: zero, stuck at zero, and the tool gets zero.
		Mode: "disable", WantSoft: 0, WantRaised: false, WantToolZero: true,
	}, { // Test 1: Not disabled: the raised limit stays, can be raised, and reaches the tool.
		Mode: "keep", WantSoft: raisedLimit, WantRaised: true, WantToolZero: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), childEnv+"="+test.Mode)
			out, err := cmd.Output()
			line := strings.TrimSpace(string(out))
			if err != nil || strings.HasPrefix(line, "ERR") {
				if strings.Contains(line, "too low to show") {
					t.Skip(line)
				}
				t.Fatalf("child %s: %q, %v", test.Mode, line, err)
			}
			var soft, hard uint64
			var raised bool
			var tool string
			if _, err := fmt.Sscanf(line, "soft=%d hard=%d raised=%t tool=%s", &soft, &hard, &raised,
				&tool); err != nil {
				t.Fatalf("child reported %q: %v", line, err)
			}
			got := []any{soft, raised, tool == "0"}
			want := []any{test.WantSoft, test.WantRaised, test.WantToolZero}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("child %s limits (-want +got):\n%s", test.Mode, diff)
			}
			if test.Mode == "disable" && hard != 0 {
				t.Errorf("hard core limit after Disable = %d, want 0", hard)
			}
		})
	}
}
