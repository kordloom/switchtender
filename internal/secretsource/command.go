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
// multi-line secrets survive. Stderr feeds the error only.
func resolveCommand(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%w: %s", ErrResolve, msg)
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}
