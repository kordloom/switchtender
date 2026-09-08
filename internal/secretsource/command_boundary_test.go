package secretsource

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// skipWithoutSh skips a test on a platform where the command source cannot run its config, since
// the resolver always runs it through sh.
func skipWithoutSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the command source runs its config through sh")
	}
}

// TestResolveCommandTrimsTrailingNewlinesAndKeepsEverythingElse pins the exact shape of the value a
// fetch command produces. The trim exists because command line tools add a newline, but a secret is
// compared byte for byte by whatever it authenticates to, so trimming one character too many or too
// few turns a working credential into a rejected one, and the failure appears at the remote host
// rather than here.
//
//nolint:funlen // Test function.
func TestResolveCommandTrimsTrailingNewlinesAndKeepsEverythingElse(t *testing.T) {
	t.Parallel()
	skipWithoutSh(t)
	tests := []struct {
		// Command is the shell the source runs.
		Command string
		// WantValue is the resolved secret.
		WantValue string
		// Why explains the case.
		Why string
	}{{ // Test 0: The newline a command line tool adds is removed.
		Command: `printf 'hunter2\n'`, WantValue: "hunter2", Why: "one trailing newline",
	}, { // Test 1: A value with no trailing newline is unchanged.
		Command: `printf 'hunter2'`, WantValue: "hunter2", Why: "no trailing newline",
	}, { // Test 2: A Windows line ending is removed whole.
		Command: `printf 'hunter2\r\n'`, WantValue: "hunter2", Why: "a CRLF ending",
	}, { // Test 3: Interior newlines survive, so a multi-line secret such as a PEM key resolves whole.
		Command: `printf 'line1\nline2\nline3\n'`, WantValue: "line1\nline2\nline3",
		Why: "interior newlines",
	}, { // Test 4: Every trailing newline is removed, not only the last one.
		Command: `printf 'hunter2\n\n\n'`, WantValue: "hunter2", Why: "several trailing newlines",
	}, { // Test 5: A value that is only newlines resolves to nothing at all.
		Command: `printf '\n\n'`, WantValue: "", Why: "a value of only newlines",
	}, { // Test 6: Trailing spaces and tabs are kept, since only newlines are trimmed.
		Command: `printf 'hunter2  \t'`, WantValue: "hunter2  \t", Why: "trailing whitespace",
	}, { // Test 7: A trailing space before the newline survives the trim.
		Command: `printf 'hunter2 \n'`, WantValue: "hunter2 ", Why: "a space before the newline",
	}, { // Test 8: Leading whitespace is untouched.
		Command: `printf '  hunter2\n'`, WantValue: "  hunter2", Why: "leading whitespace",
	}, { // Test 9: An interior carriage return survives.
		Command: `printf 'a\rb'`, WantValue: "a\rb", Why: "an interior carriage return",
	}, { // Test 10: Non-ASCII secrets survive byte for byte.
		Command: `printf 'pässwörd-✓-日本\n'`, WantValue: "pässwörd-✓-日本", Why: "a unicode secret",
	}, { // Test 11: A one-character secret resolves.
		Command: `printf 'x'`, WantValue: "x", Why: "a one-character secret",
	}, { // Test 12: A command that prints nothing resolves to nothing.
		Command: `true`, WantValue: "", Why: "no output at all",
	}, { // Test 13: A PEM key round-trips with its structure intact.
		Command:   `printf -- '-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----\n'`,
		WantValue: "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----",
		Why:       "a PEM private key",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := resolveCommand(context.Background(), test.Command)
			if err != nil {
				t.Fatalf("resolveCommand with %s: %v", test.Why, err)
			}
			if diff := cmp.Diff(test.WantValue, got); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", test.Why, diff)
			}
		})
	}
}

// TestResolveCommandCarriesALongSecretWhole checks the resolver does not cut a large secret short.
// A certificate bundle or a kubeconfig fetched from a store is tens of kilobytes, and a partial one
// authenticates nowhere while looking like a successful resolve.
func TestResolveCommandCarriesALongSecretWhole(t *testing.T) {
	t.Parallel()
	skipWithoutSh(t)
	for testNum, size := range []int{1, 4095, 4096, 4097, 65536, 200000} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := resolveCommand(context.Background(),
				fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'A'", size))
			if err != nil {
				t.Fatalf("resolveCommand: %v", err)
			}
			if diff := cmp.Diff(size, len(got)); diff != "" {
				t.Errorf("length mismatch (-want +got):\n%s", diff)
			}
			if strings.Trim(got, "A") != "" {
				t.Error("the resolved value contains bytes the command never printed")
			}
		})
	}
}

// TestResolveCommandFailsClosedOnEveryFailureShape pins that no failure of the fetch command yields
// a value. The command is the one source whose config is shell an operator wrote, so it fails in
// more ways than an HTTP call: a non-zero exit, a signal, a command that is not installed, a syntax
// error. Each has to be a refusal, because returning what reached stdout before the failure would
// hand the run a half-written secret.
func TestResolveCommandFailsClosedOnEveryFailureShape(t *testing.T) {
	t.Parallel()
	skipWithoutSh(t)
	tests := []struct {
		// Command is the shell the source runs.
		Command string
		// Why explains the failure shape.
		Why string
	}{{ // Test 0: A plain non-zero exit.
		Command: "exit 1", Why: "a non-zero exit",
	}, { // Test 1: The highest ordinary exit status.
		Command: "exit 255", Why: "exit status 255",
	}, { // Test 2: A value printed before the failure is not returned.
		Command: "printf 'partial-secret'; exit 2", Why: "output before a failure",
	}, { // Test 3: The command is killed by a signal.
		Command: "kill -TERM $$", Why: "a fatal signal",
	}, { // Test 4: The fetch tool is not installed on the runner.
		Command: "/nonexistent/fetch-secret", Why: "a missing command",
	}, { // Test 5: The operator's shell has a syntax error.
		Command: "if then fi", Why: "a shell syntax error",
	}, { // Test 6: A pipeline whose last stage fails.
		Command: "printf 'x' | exit 3", Why: "a failing pipeline",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := resolveCommand(context.Background(), test.Command)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("%s = %q, %v; want ErrResolve", test.Why, got, err)
			}
			if got != "" {
				t.Errorf("%s returned the value %q alongside its error", test.Why, got)
			}
		})
	}
}

// TestResolveCommandStopsWhenItsContextEnds pins that a fetch command is bounded by the caller's
// context. The command is arbitrary shell, so a store that hangs, or a tool that waits on a prompt
// nobody will answer, would otherwise hold the run's worker for as long as the process lives.
func TestResolveCommandStopsWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	skipWithoutSh(t)

	// Test 0: A context already canceled refuses without running the command to completion.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := resolveCommand(canceled, "printf 'should-not-matter'")
	if !errors.Is(err, ErrResolve) {
		t.Errorf("canceled context = %q, %v; want ErrResolve", got, err)
	}

	// Test 1: A command that outlives its deadline is killed and the resolve refuses promptly.
	ctx, cancelDeadline := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelDeadline()
	start := time.Now()
	got, err = resolveCommand(ctx, "sleep 30")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrResolve) {
		t.Errorf("expired deadline = %q, %v; want ErrResolve", got, err)
	}
	if got != "" {
		t.Errorf("expired deadline returned the value %q", got)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the resolve took %v against a 200ms deadline, so a hung fetch holds the run", elapsed)
	}
}

// TestResolveCommandBoundsItsOutput demonstrates a real bug. Every HTTP resolver caps the response
// it reads at one mebibyte, with the reason stated on httpMaxBody: a misbehaving endpoint must not
// exhaust memory. The command source reads its subprocess's stdout into an unbounded buffer, so a
// fetch command that streams without end, whether misconfigured or pointed at the wrong file, grows
// the server's heap until it dies. Runs in flight, and the audit trail they were about to write, go
// down with it.
func TestResolveCommandBoundsItsOutput(t *testing.T) {
	t.Parallel()
	skipWithoutSh(t)

	// Well past the cap the HTTP resolvers enforce, and still far short of what a runaway would reach.
	const size = 8 * httpMaxBody
	got, err := resolveCommand(context.Background(),
		fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'A'", size))
	if err == nil && len(got) > httpMaxBody {
		t.Errorf("a fetch command returned %d bytes, past the %d byte bound every other resolver "+
			"enforces; nothing stops a command that never stops printing", len(got), httpMaxBody)
	}
}

// TestResolveCommandReturnsWhenAGrandchildHoldsStdout demonstrates a real bug. The resolver
// collects stdout through a pipe, and Run waits for that pipe to close rather than for the shell to
// exit, so a fetch command that leaves any background process behind blocks until the grandchild
// exits. Killing the shell on context cancellation does not close the pipe the grandchild still
// holds, so the deadline the caller set is not enforced and the run's worker is held for as long as
// the grandchild lives, which for a daemonized helper is forever.
func TestResolveCommandReturnsWhenAGrandchildHoldsStdout(t *testing.T) {
	t.Parallel()
	skipWithoutSh(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = resolveCommand(ctx, "sleep 10 & printf 'hi'")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the resolve took %v against a 200ms deadline because a background process still "+
			"held stdout; the run's worker is held for the grandchild's whole lifetime", elapsed)
	}
}

// TestResolveCommandRunsThroughTheRegistryLikeAnyOtherKind pins that the command kind reaches the
// command resolver, and that a command config is not treated as JSON or as a literal value. A
// command source whose config fell through to the local kind would store the shell as the secret
// and hand it to the run.
func TestResolveCommandRunsThroughTheRegistryLikeAnyOtherKind(t *testing.T) {
	t.Parallel()
	skipWithoutSh(t)
	value, lease, err := ResolveLeased(context.Background(), KindCommand, `printf 'from-the-command'`)
	if err != nil {
		t.Fatalf("ResolveLeased: %v", err)
	}
	if diff := cmp.Diff("from-the-command", value); diff != "" {
		t.Errorf("value mismatch (-want +got):\n%s", diff)
	}
	if lease != nil {
		t.Error("the command kind returned a lease, which cleanup would try to revoke")
	}

	// The local kind, by contrast, returns its config untouched rather than running it.
	value, _, err = ResolveLeased(context.Background(), KindLocal, `printf 'from-the-command'`)
	if err != nil || value != `printf 'from-the-command'` {
		t.Errorf("local = %q, %v; want the config verbatim, never executed", value, err)
	}
}
