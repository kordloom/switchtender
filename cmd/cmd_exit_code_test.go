package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// executeArgsEnv names the environment variable that turns a re-executed copy of this test binary
// into the CLI itself. Execute ends the process with os.Exit, so the only honest way to observe the
// code it chooses is to run it in a child and read the child's status.
const executeArgsEnv = "SWITCHTENDER_TEST_EXECUTE_ARGS"

// executeArgsSep separates the arguments packed into executeArgsEnv. It is a unit separator so an
// argument containing a space, a comma, or a newline still arrives as one argument.
const executeArgsSep = "\x1f"

// TestMain runs the ordinary test suite, unless the process was started as the CLI-under-test, in
// which case it hands control to Execute and never returns.
func TestMain(m *testing.M) {
	if raw, ok := os.LookupEnv(executeArgsEnv); ok {
		args := []string{"switchtender"}
		if raw != "" {
			args = append(args, strings.Split(raw, executeArgsSep)...)
		}
		os.Args = args
		// Execute always ends the process, so nothing after this line runs.
		Execute(nil)
	}
	// Walk the command tree once, single-threaded, before any parallel test runs. Cobra sorts a
	// command's children on the first Commands() call and caches the result, so that first call is
	// a write. Parallel tests reaching the tree at the same time raced on it, and the detector
	// failed whichever test happened to be running, so the suite went red at random under load.
	warmCommandTree(rootCmd)
	os.Exit(m.Run())
}

// warmCommandTree forces cobra's lazy child sort for every command, so later reads are pure reads.
func warmCommandTree(c *cobra.Command) {
	for _, kid := range c.Commands() {
		warmCommandTree(kid)
	}
}

// runCLI runs the CLI in a child process and reports what it wrote and the code it exited with. The
// child runs in a fresh temporary directory so a command that falls back to the default database
// path cannot write into the repository.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	child := exec.Command(os.Args[0])
	child.Dir = t.TempDir()
	child.Env = append(os.Environ(), executeArgsEnv+"="+strings.Join(args, executeArgsSep))
	var out, errOut strings.Builder
	child.Stdout, child.Stderr = &out, &errOut
	err := child.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case asExitError(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("running the CLI with %v failed to start: %v", args, err)
	}
	return out.String(), errOut.String(), code
}

// asExitError reports whether err is an *exec.ExitError and stores it in target.
func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// TestExecuteReturnsSuccessForTheCommandsThatSucceed pins the zero exit. A wrapper script, a systemd
// unit, and a CI step all branch on this, so a command that did its job and exited nonzero would
// look like a failed deploy, and the reverse would hide a real one.
func TestExecuteReturnsSuccessForTheCommandsThatSucceed(t *testing.T) {
	tests := []struct {
		Name string
		Args []string
	}{{ // Test 0: No arguments prints the command list rather than failing.
		Name: "no arguments", Args: nil,
	}, { // Test 1: Asking for help is not an error.
		Name: "root help", Args: []string{"--help"},
	}, { // Test 2: A subcommand's help is not an error.
		Name: "subcommand help", Args: []string{"token", "--help"},
	}, { // Test 3: version prints and succeeds without touching the network.
		Name: "version", Args: []string{"version"},
	}, { // Test 4: A group command with no body of its own prints its subcommands.
		Name: "group command", Args: []string{"audit"},
	}, { // Test 5: The help subcommand is not an error.
		Name: "help subcommand", Args: []string{"help", "serve"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case forks a copy of this binary, and a burst of forks
			// starves the timing-sensitive tests running alongside it.
			_, stderr, code := runCLI(t, test.Args...)
			if code != CodeOK {
				t.Errorf("%s: switchtender %v exited %d, want %d. stderr:\n%s",
					test.Name, test.Args, code, CodeOK, stderr)
			}
		})
	}
}

// TestExecuteReportsUsageErrorsWithTheUsageCode pins the one path that reaches CodeUsage today: a
// refusal a command body raises and wraps in ErrUsage. Exit 2 is what a caller reads as "you typed
// it wrong" rather than "it tried and failed", and a negative token lifetime is exactly that: the
// operator meant a short-lived credential and would otherwise be handed one that never expires.
func TestExecuteReportsUsageErrorsWithTheUsageCode(t *testing.T) {
	tests := []struct {
		Name string
		Args []string
	}{{ // Test 0: A negative lifetime given as a separate argument.
		Name: "separate value", Args: []string{"token", "new", "--ttl", "-1h"},
	}, { // Test 1: The same refusal when the value is attached to the flag.
		Name: "attached value", Args: []string{"token", "new", "--ttl=-30m"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case forks a copy of this binary, and a burst of forks
			// starves the timing-sensitive tests running alongside it.
			stdout, stderr, code := runCLI(t, test.Args...)
			if code != CodeUsage {
				t.Errorf("%s: switchtender %v exited %d, want the usage code %d. stderr:\n%s",
					test.Name, test.Args, code, CodeUsage, stderr)
			}
			if !strings.Contains(stderr, "invalid usage") {
				t.Errorf("%s: stderr = %q, want it to name the usage failure", test.Name, stderr)
			}
			if stdout != "" {
				t.Errorf("%s: a refused command wrote %q to stdout, which would corrupt a pipe",
					test.Name, stdout)
			}
		})
	}
}

// TestExecuteReportsRuntimeFailuresWithTheErrorCode pins the nonzero exit for a command that was
// typed correctly and then could not do the job. These must never come back zero: a CI step that
// verifies a receipt and a cron line that anchors a chain both decide whether to page on this
// number alone, so a failure exiting zero is a check that silently stopped checking.
func TestExecuteReportsRuntimeFailuresWithTheErrorCode(t *testing.T) {
	tests := []struct {
		Name string
		Args []string
	}{{ // Test 0: A receipt file that is not there cannot be verified.
		Name: "missing receipt", Args: []string{"verify", "/nonexistent/receipt.json"},
	}, { // Test 1: A malformed audit receipt is refused before any store is opened.
		Name: "malformed receipt", Args: []string{"audit", "receipt", "not-a-receipt"},
	}, { // Test 2: An attestation file that is not there cannot be verified.
		Name: "missing attestation",
		Args: []string{"witness", "verify-attestation", "/nonexistent/att.json"},
	}, { // Test 3: A witness with nothing to watch is a misconfiguration.
		Name: "witness without a server", Args: []string{"witness"},
	}, { // Test 4: An export file that is not there cannot be imported.
		Name: "missing export", Args: []string{"import", "awx", "/nonexistent/export.json"},
	}, { // Test 5: A license file that is not there cannot be installed.
		Name: "missing license", Args: []string{"license", "install", "/nonexistent/lic.json"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case forks a copy of this binary, and a burst of forks
			// starves the timing-sensitive tests running alongside it.
			stdout, stderr, code := runCLI(t, test.Args...)
			if code != CodeError {
				t.Errorf("%s: switchtender %v exited %d, want the error code %d. stderr:\n%s",
					test.Name, test.Args, code, CodeError, stderr)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Errorf("%s: the command failed silently; nothing was written to stderr", test.Name)
			}
			if stdout != "" {
				t.Errorf("%s: a failed command wrote %q to stdout, which would corrupt a pipe",
					test.Name, stdout)
			}
		})
	}
}

// TestExecuteNeverExitsZeroOnAMalformedInvocation pins that a bad flag, an unknown command, or the
// wrong number of arguments is a failure. The exact code is asserted separately; what must hold
// unconditionally is that none of them looks like success to a caller.
func TestExecuteNeverExitsZeroOnAMalformedInvocation(t *testing.T) {
	tests := []struct {
		Name string
		Args []string
	}{{ // Test 0: A flag the binary does not have.
		Name: "unknown root flag", Args: []string{"--bogus"},
	}, { // Test 1: A flag the subcommand does not have.
		Name: "unknown subcommand flag", Args: []string{"version", "--bogus"},
	}, { // Test 2: A command that does not exist.
		Name: "unknown command", Args: []string{"nosuchcommand"},
	}, { // Test 3: A subcommand that does not exist under a group that runs itself.
		Name: "unknown subcommand of a runnable group", Args: []string{"witness", "nosuchsubcommand"},
	}, { // Test 4: A command that requires an argument, given none.
		Name: "missing argument", Args: []string{"verify"},
	}, { // Test 5: A command that takes one argument, given two.
		Name: "extra argument", Args: []string{"verify", "a", "b"},
	}, { // Test 6: A command that takes no arguments, given one.
		Name: "unexpected argument", Args: []string{"examples", "unexpected"},
	}, { // Test 7: A duration flag given something that is not a duration.
		Name: "unparsable duration", Args: []string{"token", "new", "--ttl", "soon"},
	}, { // Test 8: An integer flag given something that is not an integer.
		Name: "unparsable integer", Args: []string{"audit", "bundle", "--limit", "many"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case forks a copy of this binary, and a burst of forks
			// starves the timing-sensitive tests running alongside it.
			_, stderr, code := runCLI(t, test.Args...)
			if code == CodeOK {
				t.Errorf("%s: switchtender %v exited 0, so a malformed invocation looks like success. "+
					"stderr:\n%s", test.Name, test.Args, stderr)
			}
		})
	}
}

// TestMalformedInvocationsShareTheRuntimeErrorCode records that cobra's own refusals exit 1 rather
// than 2, so a caller cannot tell a mistyped command from a command that ran and failed.
//
// This is pinned as current behavior rather than asserted as correct. CodeUsage is documented as
// "a CLI usage error", and the root command's own comment names a bad flag or a wrong argument count
// as the case where the manual is the answer, which is why cobra still prints usage for them. They
// nonetheless come back as CodeError, while the one refusal a command body raises itself comes back
// as CodeUsage. A wrapper that branches on 2 to re-print help catches the hand-written case and
// misses every malformed invocation.
func TestMalformedInvocationsShareTheRuntimeErrorCode(t *testing.T) {
	tests := []struct {
		Name string
		Args []string
	}{{ // Test 0: An unknown flag.
		Name: "unknown flag", Args: []string{"--bogus"},
	}, { // Test 1: An unknown command.
		Name: "unknown command", Args: []string{"nosuchcommand"},
	}, { // Test 2: The wrong number of arguments.
		Name: "missing argument", Args: []string{"verify"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case forks a copy of this binary, and a burst of forks
			// starves the timing-sensitive tests running alongside it.
			_, stderr, code := runCLI(t, test.Args...)
			if code != CodeError {
				t.Errorf("%s: switchtender %v exited %d, want the currently pinned %d. If this "+
					"changed to %d deliberately, this test is the one to update. stderr:\n%s",
					test.Name, test.Args, code, CodeError, CodeUsage, stderr)
			}
		})
	}
}

// TestGroupCommandsRefuseAnUnknownSubcommand proves a mistyped subcommand under a group command
// exits zero, which reports success for work that never happened.
//
// Every group that has no body of its own (audit, token, user, license, import) hands an
// unrecognized argument to cobra, which prints the group's help to stdout and returns no error, so
// Execute exits 0. The root command refuses an unknown command, and a group that is itself runnable
// refuses one through its Args validator, so the silence is confined to exactly the groups that are
// only containers.
//
// Two cases make this more than untidy. "switchtender token revok tok_abc --db st.db", a mistyped
// revocation of a leaked credential, prints help, exits 0, and leaves the token live; the operator
// and any script around them read that as the credential being gone. And "switchtender audit anchr",
// a mistyped anchor in the cron line the product tells operators to schedule, exits 0 and anchors
// nothing forever, while the monitoring watching that exit code reports the job healthy. A
// subcommand renamed across an upgrade takes the same shape: the job keeps passing and stops working.
//
// The group's own persistent flags parse fine, which is what lets a realistic command line through:
// --db and --pretty belong to the token group, so only the subcommand name is unrecognized and
// nothing objects to it.
//
// A group command that carried Args: cobra.NoArgs would refuse the argument instead. Skipped so the
// suite stays green; remove the skip once the groups refuse.
func TestGroupCommandsRefuseAnUnknownSubcommand(t *testing.T) {
	tests := []struct {
		Name string
		Args []string
	}{{ // Test 0: A mistyped revocation of a credential that needs revoking.
		Name: "token revok", Args: []string{"token", "revok", "tok_abc", "--db", "st.db"},
	}, { // Test 1: The scheduled anchor command, mistyped.
		Name: "audit anchr", Args: []string{"audit", "anchr"},
	}, { // Test 2: A mistyped listing, with the group's own flags parsing cleanly.
		Name: "token lst", Args: []string{"token", "lst", "--db", "st.db", "--pretty"},
	}, { // Test 3: Any unknown subcommand of the audit group.
		Name: "audit unknown", Args: []string{"audit", "nosuchsubcommand"},
	}, { // Test 4: The user group.
		Name: "user unknown", Args: []string{"user", "nosuchsubcommand"},
	}, { // Test 5: The license group.
		Name: "license unknown", Args: []string{"license", "nosuchsubcommand"},
	}, { // Test 6: The import group.
		Name: "import unknown", Args: []string{"import", "nosuchsubcommand"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case forks a copy of this binary, and a burst of forks
			// starves the timing-sensitive tests running alongside it.
			_, stderr, code := runCLI(t, test.Args...)
			if code == CodeOK {
				t.Errorf("%s: switchtender %v exited 0, so a job that never ran reports success. "+
					"stderr:\n%s", test.Name, test.Args, stderr)
			}
		})
	}
}

// TestVersionPrintsToStdoutAndNothingElse pins where the version goes. It is the value a release
// pipeline captures with command substitution, so a stray line on stdout would be read as part of
// the version string.
func TestVersionPrintsToStdoutAndNothingElse(t *testing.T) {
	stdout, stderr, code := runCLI(t, "version")
	if code != CodeOK {
		t.Fatalf("switchtender version exited %d, want %d. stderr:\n%s", code, CodeOK, stderr)
	}
	got := strings.TrimSpace(stdout)
	if got == "" {
		t.Fatal("switchtender version printed nothing to stdout")
	}
	if strings.Contains(got, "\n") {
		t.Errorf("switchtender version printed %d lines to stdout, want exactly one:\n%s",
			strings.Count(got, "\n")+1, got)
	}
	if got != resolveVersion() {
		t.Errorf("switchtender version printed %q, want %q", got, resolveVersion())
	}
}
