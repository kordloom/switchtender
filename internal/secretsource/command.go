package secretsource

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

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
func resolveCommand(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrResolve, err)
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}
