package audit

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/merkle"
	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/internal/util"
	"github.com/kordloom/switchtender/witness"
)

// attestationMembers are the members a LoomSeal attestation object may carry, by their exact names.
var attestationMembers = memberSet("alg", "at", "key_id", "public_key", "role", "sig")

// witnessTally is what one witness key's verified attestations in a bundle cover.
type witnessTally struct {
	// seqs holds the seq of every entry the key countersigned.
	seqs map[int64]bool
	// newest is the highest of seqs.
	newest int64
}

// refuseForeignAttestations refuses, by name, every attestation on claim i that this product does
// not verify: one that is not an object of LoomSeal attestation members with string values, and one
// whose role is not witness. A witness attestation passes here and is verified by
// verifyWitnessAttestations, so nothing an attestation carries reaches a verdict unchecked.
func refuseForeignAttestations(i int, atts []json.RawMessage) error {
	for _, raw := range atts {
		obj, err := attestationObject(raw)
		if err != nil {
			return fmt.Errorf("%w: claim %d carries an attestation that is not a LoomSeal "+
				"attestation (%w): this product does not verify it; use the open loomseal verifier",
				ErrVerify, i, err)
		}
		if role := obj["role"]; role != witness.RoleWitness {
			return fmt.Errorf("%w: claim %d carries an attestation of role %q: this product does "+
				"not verify it; use the open loomseal verifier", ErrVerify, i, role)
		}
	}
	return nil
}

// attestationObject reads one attestation member as the object of strings LoomSeal defines,
// refusing a duplicate member, a member outside the set by its exact name, and a value that is not
// a string.
// Reading the canonical tree rather than decoding into a struct is what keeps a case variant of a
// member from folding into the field a check reads.
func attestationObject(raw json.RawMessage) (map[string]string, error) {
	value, err := jcs.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	out := make(map[string]string, len(obj))
	for name, v := range obj {
		if !attestationMembers[name] {
			return nil, fmt.Errorf("member %q is not an attestation member", name)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("member %q is not a string", name)
		}
		out[name] = s
	}
	return out, nil
}

// verifyWitnessAttestations verifies every witness attestation a claim carries under the LoomSeal
// rule: the key id is the digest of the key, and the key's ed25519 signature covers the canonical
// attestation object naming that claim's link. One that verifies is counted under its key, once per
// key and entry. One that does not is a problem that fails the document, since either the claim it
// rides on was altered or the attestation was forged. refuseForeignAttestations has already refused
// every attestation that is not a witness's.
func verifyWitnessAttestations(claims []BundleClaim, rep *BundleReport) {
	for _, c := range claims {
		for _, raw := range c.Attestations {
			obj, err := attestationObject(raw)
			if err != nil {
				rep.WitnessProblems = append(rep.WitnessProblems, fmt.Sprintf(
					"the witness attestation on the entry at seq %d is out of form: %v", c.Chain.Seq,
					err))
				continue
			}
			cs := witness.Countersignature{
				KeyID: obj["key_id"], PublicKey: obj["public_key"], Alg: obj["alg"],
				Role: obj["role"], At: obj["at"], Sig: obj["sig"],
			}
			if err := witness.VerifyClaimAttestation(c.Chain.Link, cs); err != nil {
				rep.WitnessProblems = append(rep.WitnessProblems, fmt.Sprintf(
					"the witness attestation by %s on the entry at seq %d does not verify: %v",
					util.Clip(cs.KeyID, 80), c.Chain.Seq, err))
				continue
			}
			rep.tallyWitness(cs.KeyID, c.Chain.Seq)
		}
	}
	for key, t := range rep.witnessBy {
		rep.WitnessKeyIDs = append(rep.WitnessKeyIDs, key)
		rep.WitnessAttested += len(t.seqs)
		rep.WitnessNewestSeq = max(rep.WitnessNewestSeq, t.newest)
	}
	sort.Strings(rep.WitnessKeyIDs)
}

// tallyWitness records that the witness key keyID countersigned the entry at seq.
func (r *BundleReport) tallyWitness(keyID string, seq int64) {
	if r.witnessBy == nil {
		r.witnessBy = make(map[string]*witnessTally)
	}
	t := r.witnessBy[keyID]
	if t == nil {
		t = &witnessTally{seqs: make(map[int64]bool)}
		r.witnessBy[keyID] = t
	}
	t.seqs[seq] = true
	t.newest = max(t.newest, seq)
}

// WitnessedBy reports how many entries the witness key keyID countersigned in the bundle, among the
// attestations that verify, and the newest seq among them. Both are zero for a key that signed
// none.
func (r *BundleReport) WitnessedBy(keyID string) (count int, newest int64) {
	t := r.witnessBy[keyID]
	if t == nil {
		return 0, 0
	}
	return len(t.seqs), t.newest
}

// WitnessPinKeyID returns the witness key id a pin names. A pin is either form a witness publishes:
// the sha256 key id, or the raw ed25519 public key in hex, whose key id it then computes. Anything
// else is ErrVerify, since a pin that names no key would compare unequal to every signer and read
// as a forgery.
func WitnessPinKeyID(pin string) (string, error) {
	pin = strings.TrimSpace(pin)
	if rest, ok := strings.CutPrefix(pin, "sha256:"); ok {
		if raw, err := hex.DecodeString(rest); err == nil && len(raw) == 32 {
			return "sha256:" + strings.ToLower(rest), nil
		}
	}
	if raw, err := hex.DecodeString(pin); err == nil && len(raw) == 32 {
		return seal.KeyID(raw), nil
	}
	return "", fmt.Errorf("%w: witness key pin %q is neither a sha256 key id nor a hex ed25519 "+
		"public key", ErrVerify, util.Clip(pin, 80))
}

// StatementState is the verdict of holding a bundle to a witness statement.
type StatementState string

// The verdicts a witness statement check reaches.
const (
	// StatementHeld is a bundle that reproduces the coordinate the witness countersigned, from the
	// producer key and install the statement names, with no refusal on the record.
	StatementHeld StatementState = "held"
	// StatementFailed is a statement that does not verify, is not from the pinned witness, is about
	// another key or install, records a refusal, or names a coordinate the bundle contradicts.
	StatementFailed StatementState = "failed"
	// StatementNotCheckable is a bundle that does not carry the coordinate the statement names, so
	// the statement says nothing about it either way.
	StatementNotCheckable StatementState = "not checkable"
	// StatementNothingWitnessed is a signed statement of size 0: the witness holds no countersigned
	// head for the install yet, so it witnesses nothing.
	StatementNothingWitnessed StatementState = "nothing witnessed"
)

// StatementCheck is what holding a bundle to a witness statement showed.
type StatementCheck struct {
	// State is the verdict.
	State StatementState
	// Problem says why the state is not StatementHeld, empty when it is.
	Problem string
	// Pinned reports the statement's signer was compared with a pinned witness key and matched.
	Pinned bool
	// SignerKeyID is the witness key id that signed the statement, set once the signature verifies.
	SignerKeyID string
	// LastSeq is the newest entry the statement says the witness countersigned.
	LastSeq int64
	// Size is the tree size the statement says the witness holds.
	Size int64
	// RootCompared reports the bundle held every entry from seq 1 through Size, so the tree root
	// over them was compared with the statement's. A window into a longer chain is held to the link
	// at LastSeq alone, which commits to every entry before it.
	RootCompared bool
	// RefusalsTotal is how many pushes the witness refused from the install.
	RefusalsTotal int64
	// NotesTotal is how many notes the witness made about its coverage of the install. A note never
	// fails a statement.
	NotesTotal int64
}

// OK reports whether the bundle holds against the statement.
func (c *StatementCheck) OK() bool { return c.State == StatementHeld }

// fail sets the check failed for why and returns it.
func (c *StatementCheck) fail(why string, args ...any) *StatementCheck {
	c.State, c.Problem = StatementFailed, fmt.Sprintf(why, args...)
	return c
}

// unchecked sets the check to state for why and returns it.
func (c *StatementCheck) unchecked(state StatementState, why string, args ...any) *StatementCheck {
	c.State, c.Problem = state, fmt.Sprintf(why, args...)
	return c
}

// CheckWitnessStatement holds the bundle signed, which VerifyBundle reported as rep, to a witness's
// signed statement of what it holds for one install.
//
// In order: the statement's signature verifies and, when pin is set, its signer is the pinned key
// in either published form; the statement is about the producer key that signed the bundle, which
// is compared before the install label because the witness keeps its memory under the key, and a
// statement for another key is a wrong statement rather than a rewritten chain; the statement's
// install is the bundle's; the witness refused no push from the install, since a refusal is its
// testimony that the install's signed account contradicted itself; and the bundle reproduces the
// newest coordinate the witness countersigned, the link at LastSeq and, when the bundle holds every
// entry through Size, the tree root under the install's label. A size 0 statement witnesses
// nothing, and a bundle that does not carry the entry at LastSeq is not checkable against the
// statement. Neither is ever held, and notes never fail it.
func CheckWitnessStatement(rep *BundleReport, signed []byte, st *witness.Statement, pin string) *StatementCheck {
	c := &StatementCheck{State: StatementFailed}
	if st == nil {
		return c.fail("no witness statement was given")
	}
	c.LastSeq, c.Size, c.RefusalsTotal, c.NotesTotal = st.LastSeq, st.Size, st.RefusalsTotal,
		st.NotesTotal
	signer, err := witness.VerifyStatement(st)
	if err != nil {
		return c.fail("the witness statement does not verify: %v", err)
	}
	c.SignerKeyID = signer
	if pin != "" {
		want, err := WitnessPinKeyID(pin)
		if err != nil {
			return c.fail("%v", err)
		}
		if signer != want {
			return c.fail("the witness statement is signed by %s, not the pinned witness key %s",
				signer, want)
		}
		c.Pinned = true
	}
	if rep == nil {
		return c.fail("the bundle was not verified, so there is nothing to hold to the statement")
	}
	if st.KeyID != rep.KeyID {
		return c.fail("witness statement is for key %s, bundle is signed by %s", st.KeyID,
			rep.KeyID)
	}
	var b Bundle
	if err := json.Unmarshal(signed, &b); err != nil {
		return c.fail("the bundle does not parse: %v", err)
	}
	if st.InstallID != b.Producer.InstallID {
		return c.fail("the witness statement is about install %s, and this bundle names install %s",
			util.Clip(st.InstallID, 80), b.Producer.InstallID)
	}
	if st.RefusalsTotal < 0 || st.NotesTotal < 0 {
		return c.fail("the witness statement counts %d refusals and %d notes, which no witness "+
			"signs", st.RefusalsTotal, st.NotesTotal)
	}
	if st.RefusalsTotal > 0 {
		pushes := "pushes"
		if st.RefusalsTotal == 1 {
			pushes = "push"
		}
		return c.fail("the witness refused %d %s from this install, see GET "+
			"/v1/installs/%s/findings", st.RefusalsTotal, pushes, st.KeyID)
	}
	if st.Size == 0 {
		if st.LastBeat != 0 || st.LastSeq != 0 || st.LastHead != "" || st.Root != "" {
			return c.fail("the witness statement holds a tree of 0 entries and names beat %d at "+
				"entry %d, which no witness signs", st.LastBeat, st.LastSeq)
		}
		return c.unchecked(StatementNothingWitnessed, "nothing witnessed: the witness signed that "+
			"it holds no countersigned head for install %s yet", b.Producer.InstallID)
	}
	if st.Size < 1 || st.LastSeq < 1 || st.LastHead == "" || st.Root == "" {
		return c.fail("the witness statement names tree size %d and entry %d without both a head "+
			"and a root, which no witness signs", st.Size, st.LastSeq)
	}
	return holdToCoordinate(c, rep, &b, st)
}

// holdToCoordinate finishes a statement check whose statement is about this bundle's producer and
// records no refusal, by holding the bundle to the coordinate the statement names.
func holdToCoordinate(c *StatementCheck, rep *BundleReport, b *Bundle, st *witness.Statement) *StatementCheck {
	if b.Chain != nil && b.Chain.Profile == TreeProfile {
		return c.unchecked(StatementNotCheckable, "this is a tree bundle, whose entries carry leaf "+
			"hashes rather than the chain links a witness countersigns, so it is not checkable "+
			"against this statement")
	}
	if !rep.SignatureOK || !rep.ChainOK {
		return c.unchecked(StatementNotCheckable, "the bundle's own signature or chain does not "+
			"verify, so its links are not checkable against this statement")
	}
	bySeq := make(map[int64]*BundleClaim, len(b.Claims))
	var last int64
	for i := range b.Claims {
		bySeq[b.Claims[i].Chain.Seq] = &b.Claims[i]
		last = max(last, b.Claims[i].Chain.Seq)
	}
	if last < st.LastSeq {
		return c.unchecked(StatementNotCheckable, "this bundle ends at seq %d, before seq %d, the "+
			"newest entry the witness countersigned, so it is not checkable against this "+
			"statement", last, st.LastSeq)
	}
	at := bySeq[st.LastSeq]
	if at == nil {
		return c.unchecked(StatementNotCheckable, "this bundle does not carry seq %d, the newest "+
			"entry the witness countersigned, so it is not checkable against this statement",
			st.LastSeq)
	}
	if at.Chain.Link != st.LastHead {
		return c.fail("this bundle's entry at seq %d has link %s, and the witness countersigned %s "+
			"there, so the chain was rewritten at or before seq %d", st.LastSeq, at.Chain.Link,
			util.Clip(st.LastHead, 80), st.LastSeq)
	}
	// The statement chooses Size, so the leaves are gathered only when the bundle holds that many
	// entries, and never allocated from the statement's word alone.
	var leaves [][]byte
	for seq := int64(1); st.Size <= last && seq <= st.Size; seq++ {
		claim := bySeq[seq]
		if claim == nil {
			leaves = nil
			break
		}
		leaf, err := treeLeafFor(*claim, b.Producer.InstallID)
		if err != nil {
			return c.fail("the entry at seq %d does not hash into a tree leaf: %v", seq, err)
		}
		leaves = append(leaves, leaf)
	}
	if leaves != nil {
		c.RootCompared = true
		if root := hex.EncodeToString(merkle.Root(leaves)); root != st.Root {
			return c.fail("the tree over this bundle's first %d entries has root %s, and the "+
				"witness holds %s, so the chain was rewritten at or before seq %d", st.Size, root,
				util.Clip(st.Root, 80), st.Size)
		}
	}
	c.State, c.Problem = StatementHeld, ""
	return c
}
