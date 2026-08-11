package cmd

import (
	"context"
	"fmt"
	osuser "os/user"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// actorTypeCLI marks an entry made from the command line on the host, where the identity available
// is the account running the binary rather than a token or a session.
const actorTypeCLI = "cli"

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
	return recordCLIChange(ctx, audits, command, nil)
}

// recordCLIChange records a command-line mutation and, when body is non-empty, commits a content
// digest of it into the chain the same way an HTTP mutation commits the change it made. body must
// carry no secret: the digest is recomputable by any holder of the exported chain, so it is meant for
// a summary such as counts and timestamps, not a payload. A nil body records the call alone, which is
// what recordCLI does for a mutation whose content is not summarized.
func recordCLIChange(ctx context.Context, audits audit.Store, command string, body []byte) error {
	if audits == nil {
		return nil
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: cliActor(),
		// A command-line mutation is made by whoever holds the host account, which is the closest
		// thing to an observed identity here: no token and no session is involved. The type says so
		// rather than claiming a person or a service, either of which would be a guess.
		ActorType: actorTypeCLI,
		Method:    audit.MethodCLI, Path: command,
	}
	if len(body) > 0 {
		digest, nonce, err := audit.ContentDigestOf(body)
		if err != nil {
			return fmt.Errorf("refused: the change could not be recorded in the audit trail: %w", err)
		}
		entry.ContentDigest, entry.Nonce = digest, nonce
	}
	if err := audits.Append(ctx, entry); err != nil {
		return fmt.Errorf("refused: the change could not be recorded in the audit trail: %w", err)
	}
	return nil
}
