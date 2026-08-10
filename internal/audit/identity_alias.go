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
