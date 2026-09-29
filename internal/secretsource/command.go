package secretsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/util"
)

// commandWaitDelay bounds how long a resolve waits for the stdout pipe to close once the fetch
// command's own process has exited or the caller's context is done. The shell exits as soon as its
// last foreground command does, but the pipe stays open while anything the command left running in
// the background still holds the write end, and killing the shell does not close a pipe a
// grandchild holds. Without this bound the caller's deadline is not enforced and the run's worker
// is held for as long as that grandchild lives, which for a daemonized helper is forever.
const commandWaitDelay = time.Second

// errCommandOutputTooLarge stops the copy from a fetch command's stdout once the cap is reached.
// Returning it from Write closes the pipe, which stops the command rather than letting it keep
// printing into memory.
var errCommandOutputTooLarge = errors.New("command output exceeds the resolve cap")

// cappedBuffer collects a fetch command's stdout up to a fixed size and refuses everything past it.
// It is safe for concurrent use because a pipe closed on the wait delay lets the copy that fills it
// outlive the Wait that returned, while the resolve reads the buffer the moment Wait returns.
type cappedBuffer struct {
	// mu guards every field below against that surviving copy.
	mu sync.Mutex
	// buf holds the output accepted so far.
	buf bytes.Buffer
	// limit is the most bytes the buffer accepts.
	limit int
	// exceeded records that output past the limit was refused.
	exceeded bool
}

// Write appends p while the limit allows it and otherwise refuses, recording that the command
// produced more than a secret is allowed to be.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf.Len()+len(p) > c.limit {
		c.exceeded = true
		return 0, errCommandOutputTooLarge
	}
	return c.buf.Write(p)
}

// result returns the output collected so far and whether the command tried to print past the limit.
func (c *cappedBuffer) result() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String(), c.exceeded
}

// resolveCommand runs a command and returns its stdout as the value, so the real secret is fetched
// from an external store such as Vault or a cloud CLI at run time and never stored in SwitchTender. A
// trailing newline is trimmed, since command line tools add one, while interior newlines are kept so
// multi-line secrets survive.
//
// Stderr is discarded rather than surfaced. A secret-fetch command routinely prints the value it is
// fetching, or fragments of it, on the way to failing, so its stderr is untrusted secret-bearing
// output. Putting it in the returned error would land it in the run's error field, which is stored
// and served, and the masker has nothing registered when resolution itself fails, so it could not be
// redacted. Logging it would break the same rule from the other side. The caller wraps this error
// with the credential id (see openCredential) and the exit status is preserved, so the operator
// learns which source failed and how without the value leaking anywhere.
//
// Stdout is capped at the same size every HTTP resolver caps a response at, for the same reason
// stated on httpMaxBody: a misbehaving source must not exhaust memory. A fetch command that streams
// without end, whether misconfigured or pointed at the wrong file, would otherwise grow the
// server's heap until it dies, taking the runs in flight and the audit trail they were about to
// write with it. Passing the cap is a refusal rather than a truncation, since half a credential
// authenticates nowhere while looking like a successful resolve.
//
// The caller's context bounds the whole resolve, including the wait for stdout to close, which the
// shell's own exit does not guarantee. See commandWaitDelay.
//
// The command runs without SwitchTender's own configuration, the same as any tool a run executes.
// This is shell somebody configured, and the server reads its master encryption key and salt from the
// environment, so inheriting that environment let the command that fetches one credential print the
// key that protects all of them. What the command legitimately needs to reach a secret store, a Vault
// token or cloud credentials, is outside our prefix and survives.
func resolveCommand(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = util.SecretFetchEnviron()
	cmd.WaitDelay = commandWaitDelay
	stdout := &cappedBuffer{limit: httpMaxBody}
	cmd.Stdout = stdout
	err := cmd.Run()
	out, exceeded := stdout.result()
	// The cap is reported ahead of the run's own error, since refusing the write closes the pipe and
	// the command then dies of a broken pipe, whose exit status says nothing about what happened.
	if exceeded {
		return "", fmt.Errorf("%w: command output is larger than the %d byte cap",
			ErrResolve, httpMaxBody)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrResolve, err)
	}
	return strings.TrimRight(out, "\r\n"), nil
}
