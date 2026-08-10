package cmd

import (
	"context"
	"fmt"
	osuser "os/user"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// cliActor names the operator behind a command-line mutation. The account running the binary is the
// only identity available here, since no token or session is involved, and the prefix keeps it from
// being mistaken for an API token label.
func cliActor() string {
	if u, err := osuser.Current(); err == nil && u.Username != "" {
		return "cli:" + u.Username
	}
	return "cli:unknown"
}

// recordCLI appends an audit entry for a mutation about to be made from the command line, and fails
// the command when the entry cannot be written.
//
// Every mutation over HTTP is recorded by the auth middleware, but the command line reaches the same
// stores directly and bypassed it entirely. Creating an admin account and minting a token are the
// two most security-relevant operations in the product, and neither left any trace: the chain
// verified perfectly and was silent about both. A record that omits the changes most worth auditing
// is worse than no record, because it invites the reader to trust it.
//
// It is called before the change, matching the server, so a mutation that cannot be recorded does
// not happen. Every command-line mutation routes through here rather than appending its own entry,
// so a command added later either uses it or is visibly missing from this file.
func recordCLI(ctx context.Context, audits audit.Store, command string) error {
	if audits == nil {
		return nil
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: cliActor(),
		Method: audit.MethodCLI, Path: command,
	}
	if err := audits.Append(ctx, entry); err != nil {
		return fmt.Errorf("refused: the change could not be recorded in the audit trail: %w", err)
	}
	return nil
}
