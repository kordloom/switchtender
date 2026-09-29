package audit

import (
	"crypto/ed25519"

	"github.com/kordloom/switchtender/identity"
)

// installNames is the set of install ids one producer key may legitimately have written entries
// under. Every check that compares a written install id against this bundle's producer asks it, so
// the grandfathering is stated once rather than in each of them.
//
// An install id is derived from the producer key on first boot, and that derivation was widened:
// six raw key bytes became a 128-bit hash of the whole key, because 48 bits let an attacker grind
// keypairs until one was born to a chosen install id. Widening it changed what the same key calls
// the same install.
//
// For most installs that changed nothing, because the id is stored on first boot and read back
// afterward, so an upgraded install keeps the name it already had. One shape re-derives instead:
// SWITCHTENDER_AUDIT_KEY supplies the key directly and that path keeps no identity file, so the id
// is computed from the key at every boot. That is the documented way to run a shared postgres chain,
// which is the paid deployment shape. On those installs the upgrade renamed the install underneath a
// chain already written, so entries before it name one id and entries after it name another, and
// every per-entry check that demanded byte equality with the producer read the seam as tampering.
//
// A chain already written commits to what it committed to. Nothing can rewrite those entries, so the
// verifier has to know both names. It concedes nothing: the alias is derived from the very key whose
// signature is being checked, under the rule that key was born under, which the producer check
// already accepts for the same reason.
type installNames struct {
	// declared is the id the bundle's producer block names, which is what the install writes today.
	declared string
	// alias is the other name this same key derives, empty unless the declared id is one of the
	// two. A chain can hold entries under both, in either order: the rename happened when the
	// binary was upgraded, and it is undone when the install adopts the name its chain already
	// carries, so entries written between those two points name the widened id and the entries on
	// either side name the narrow one.
	alias string
}

// producerNames resolves the install ids the bundle's signing key may have written under.
func producerNames(pub ed25519.PublicKey, declared string) installNames {
	names := installNames{declared: declared}
	if declared == "" {
		return names
	}
	wide, narrow := identity.InstallIDFromKey(pub), identity.LegacyInstallIDFromKey(pub)
	switch declared {
	case wide:
		names.alias = narrow
	case narrow:
		names.alias = wide
	}
	return names
}

// matches reports whether a written install id names this producer.
func (n installNames) matches(id string) bool {
	if n.declared == "" {
		return false
	}
	return id == n.declared || (n.alias != "" && id == n.alias)
}

// installNamesOf resolves the names an identity has written under, tolerating an install that has
// no key: a process with no producer identity binds nothing and has only the name it was handed.
func installNamesOf(id Identity) installNames {
	if id.Private() == nil {
		return installNames{declared: id.InstallID}
	}
	return producerNames(id.Public(), id.InstallID)
}
