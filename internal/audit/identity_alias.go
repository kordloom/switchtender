package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kordloom/switchtender/identity"
)

// Identity is the install's signing identity. It lives in the identity package now, so a witness and
// other out-of-tree tools can sign and pin keys without importing the audit chain; this alias keeps
// the audit callers reading audit.Identity.
type Identity = identity.Identity

// IdentityFile is the file name the producer identity is stored under.
const IdentityFile = identity.File

// LoadIdentity reads the producer identity from dir, creating it on first use.
func LoadIdentity(dir string) (Identity, error) { return identity.Load(dir) }

// LoadIdentityForStore reads the producer identity for a deployment whose chain lives in db, refusing to
// invent one where inventing one is wrong.
//
// The identity lives in a file beside the database, which fits SQLite exactly: the database is a file in
// that directory, so the key and the chain it signs travel together. Against a shared database there is
// no such directory. The key was created under the operating system user's own config directory instead,
// so every replica in an active-active pair, and every host that runs an anchor job over the one shared
// chain, minted a different identity and signed as a different install. Two replicas' bundles were
// attributable to two installs, and a tree anchor taken by one could not be recomputed by the other,
// which reads to an auditor as a rewritten chain.
//
// One shared chain needs one identity, and no process can invent that on its own, so a shared store with
// no identity supplied is refused with the command that makes one. A key already in the identity
// directory, or in SWITCHTENDER_AUDIT_KEY, is used as given: that is how an operator distributes one.
func LoadIdentityForStore(db, dir string) (Identity, error) {
	if !sharedStore(db) {
		return LoadIdentity(dir)
	}
	if os.Getenv(identity.KeyEnv) != "" {
		return LoadIdentity(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, identity.File)); err == nil {
		return LoadIdentity(dir)
	}
	return Identity{}, fmt.Errorf("this install keeps its chain in a shared database and has no "+
		"signing identity, so it will not create one: every process writing that chain must sign as the "+
		"same install, and a key created here would be this host's alone. Generate one key, set "+
		"%s to it on every process, or place %s in %s on each of them",
		identity.KeyEnv, identity.File, dir)
}

// sharedStore reports whether a database address names a store several processes share, where an
// identity created on one host is not the identity the others will use.
func sharedStore(db string) bool {
	lower := strings.ToLower(db)
	return strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://")
}

// LoadWitnessIdentity reads a witness's own signing identity from dir, under its own file name and
// ignoring the environment override the producer identity honors. A witness that borrowed the watched
// server's key, from the environment or from the file beside it, would be countersigning its own
// subject: the relying party would think they had pinned an independent key.
func LoadWitnessIdentity(dir string) (Identity, error) { return identity.LoadWitnessFile(dir) }
