package audit

import "github.com/kordloom/switchtender/identity"

// Identity is the install's signing identity. It lives in the identity package now, so a witness and
// other out-of-tree tools can sign and pin keys without importing the audit chain; this alias keeps
// the audit callers reading audit.Identity.
type Identity = identity.Identity

// IdentityFile is the file name the producer identity is stored under.
const IdentityFile = identity.File

// LoadIdentity reads the producer identity from dir, creating it on first use.
func LoadIdentity(dir string) (Identity, error) { return identity.Load(dir) }

// LoadWitnessIdentity reads a witness's own signing identity from dir, under its own file name and
// ignoring the environment override the producer identity honors. A witness that borrowed the watched
// server's key, from the environment or from the file beside it, would be countersigning its own
// subject: the relying party would think they had pinned an independent key.
func LoadWitnessIdentity(dir string) (Identity, error) { return identity.LoadWitnessFile(dir) }
