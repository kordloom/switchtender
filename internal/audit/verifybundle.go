package audit

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/merkle"
	"github.com/kordloom/loomseal/seal"
	"github.com/kordloom/switchtender/identity"
)

// ErrVerify is returned when a bundle cannot be checked at all: it does not parse, or its producer
// key is not a usable key. A bundle that parses but fails a check is not an error; its report says so.
var ErrVerify = errors.New("audit verify")

// BundleReport is the verdict of checking a signed bundle offline. It reports each check separately
// so a reader sees not only that a bundle is good but which guarantees hold. A bundle is trustworthy
// when the signature verifies, the chain recomputes, and any anchors it carries match a claim.
type BundleReport struct {
	// SignatureOK reports that the producer's ed25519 signature covers the bundle unaltered.
	SignatureOK bool
	// ChainOK reports that every claim's link recomputes and the claims chain to one another.
	ChainOK bool
	// AnchorsOK reports that every carried anchor names a claim the bundle holds. True when there are
	// none, which is simply a bundle with no external fixations.
	AnchorsOK bool
	// BrokeAtSeq is the sequence of the first claim whose link did not recompute, zero when the chain
	// is whole.
	BrokeAtSeq int64
	// KeyID is the producer key fingerprint a relying party pins.
	KeyID string
	// Subject says what the bundle's claims are about.
	Subject BundleSubject
	// Head is the newest coordinate the producer attests.
	Head BundleCoord
	// TimestampsVerified counts the carried RFC 3161 tokens that were checked and found to commit to
	// the link their anchor names. Zero with anchors present means those anchors carry no embedded
	// proof, which is the ordinary git or https anchor.
	TimestampsVerified int
	// TimestampProblems names each carried token that does not fix its anchor's link.
	TimestampProblems []string
	// ClaimCount and AnchorCount report the size of what was checked.
	ClaimCount  int
	AnchorCount int
	// OutcomePresent reports that the receipt discloses a run's outcome, so a verifier can read what
	// the run did, not only that the chain around it is intact.
	OutcomePresent bool
	// OutcomeDigestOK reports that the disclosed outcome matches the digest the chain committed. A
	// receipt that discloses an outcome the chain never committed is not trustworthy.
	OutcomeDigestOK bool
	// OutcomeBody is the disclosed outcome JSON, meaningful only when OutcomeDigestOK. A caller reads
	// what the run did from it after this report confirms it matches the commitment.
	OutcomeBody []byte
	// DecisionsPresent counts the approval decisions the receipt disclosed.
	DecisionsPresent int
	// DecisionsOK reports every disclosed decision matches the digest its chain entry committed,
	// true when none are disclosed. A decision the chain does not back fails the receipt.
	DecisionsOK bool
	// Decisions are the digest-verified decisions: who decided, what they decided, and the spec
	// digest their decision bound to. Meaningful when DecisionsOK.
	Decisions []DisclosedDecision
	// SpecPresent reports the receipt disclosed the run's redacted spec.
	SpecPresent bool
	// SpecConsistent reports every possible comparison among the disclosed spec's digest, the spec
	// digest the outcome record committed, and the digest each decision bound to, agreed. True when
	// there was nothing to compare. A disagreement means the spec that executed is not the spec
	// that was approved, which is exactly what this receipt exists to rule out.
	SpecConsistent bool
	// SpecBody is the disclosed redacted spec JSON, meaningful when SpecConsistent.
	SpecBody []byte
}

// DisclosedDecision is one digest-verified approval decision read back from a receipt.
type DisclosedDecision struct {
	// Actor is who decided, as the chain committed it.
	Actor string
	// ActorType is how the decider authenticated.
	ActorType string
	// Verdict is approved or rejected.
	Verdict string
	// SpecDigest is the digest of the spec the decision bound to.
	SpecDigest string
}

// OK reports whether every check passed, the single question a verify command answers yes or no. A
// disclosed outcome or decision that does not match its commitment fails the whole receipt: it is a
// claim the chain does not back. A spec inconsistency fails it too, because then the approval and
// the execution the receipt ties together are not about the same change.
func (r *BundleReport) OK() bool {
	return r.SignatureOK && r.ChainOK && r.AnchorsOK &&
		(!r.OutcomePresent || r.OutcomeDigestOK) && r.DecisionsOK && r.SpecConsistent
}

// VerifyBundle checks a signed bundle with no store and no network: it confirms the producer's
// ed25519 signature covers the exact bytes, recomputes every chain link from the claims, and checks
// any anchors name a claim the bundle holds. It recomputes links through the same claimObject and
// linkOf that EntryHash produces them with, so a verdict here cannot drift from what the producer
// committed. When pinnedKeyID is non-empty, a bundle signed by any other key is refused before its
// signature is even checked, which is how a relying party ties trust to a key it obtained out of band
// rather than to whatever key the file names.
func VerifyBundle(signed []byte, pinnedKeyID string) (*BundleReport, error) {
	return verifyBundle(signed, pinnedKeyID, "")
}

// VerifyBundleForInstall is VerifyBundle for a relying party that has explicitly accepted a key
// rotation: the caller states, out loud, that bundles naming acceptedInstall are trusted from the
// pinned key even though the key was not the one the id was born from. Requiring both parameters is
// the point: rotation acceptance is a deliberate pairing of an install with its new key, never an
// ambient effect of trusting the key alone.
func VerifyBundleForInstall(signed []byte, pinnedKeyID, acceptedInstall string) (*BundleReport, error) {
	if pinnedKeyID == "" || acceptedInstall == "" {
		return nil, fmt.Errorf("%w: rotation acceptance requires both the pinned key and the install id",
			ErrVerify)
	}
	return verifyBundle(signed, pinnedKeyID, acceptedInstall)
}

func verifyBundle(signed []byte, pinnedKeyID, acceptedInstall string) (*BundleReport, error) {
	var b Bundle
	if err := json.Unmarshal(signed, &b); err != nil {
		return nil, fmt.Errorf("%w: parse bundle: %w", ErrVerify, err)
	}
	rep := &BundleReport{
		KeyID: b.Producer.KeyID, Subject: b.Subject,
		ClaimCount: len(b.Claims), AnchorCount: len(b.Anchors),
	}
	if b.Chain != nil {
		rep.Head = b.Chain.Head
	}
	// The advertised fingerprint has to be the fingerprint of the key that is actually embedded, and
	// this must be settled before the pin is consulted. Comparing a caller's pin against a string the
	// bundle declares proves only that the bundle claims the right author: an attacker signs with a
	// key of their own, leaves it embedded so the signature is genuine, and writes the victim's
	// published fingerprint into producer.key_id. Both halves then pass and the forgery reads as
	// verified and pinned.
	pub, err := base64.StdEncoding.DecodeString(b.Producer.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return rep, fmt.Errorf("%w: producer public key is not a valid ed25519 key", ErrVerify)
	}
	if actual := seal.KeyID(pub); actual != b.Producer.KeyID {
		return rep, fmt.Errorf("%w: bundle declares key %s but carries key %s",
			ErrVerify, b.Producer.KeyID, actual)
	}
	// The install a bundle names has to be the install that key was born to, unless the caller
	// explicitly accepted this key for this install. A bare key pin cannot stand in for that: it
	// says "I trust this key as itself," never "this key may speak for install X," and the lift
	// this check closes is exactly a genuine key presenting another install's claims, leaves, and
	// anchors as its own. A legitimately rotated install is byte-identical to that lift from the
	// bundle alone, so rotation is accepted only through acceptedInstall, the caller stating the
	// pair out loud. The install id is a stable identifier that survives key changes; derivation
	// is only its birth rule.
	//
	// A bundle naming no install predates the binding and is left alone, which is the same
	// grandfathering the per-claim check applies.
	if b.Producer.InstallID != "" && b.Producer.InstallID != acceptedInstall &&
		b.Producer.InstallID != identity.InstallIDFromKey(pub) {
		return rep, fmt.Errorf("%w: bundle names install %s, which is not the install key %s was born"+
			" to; a rotated install verifies only with its install id explicitly accepted for the"+
			" pinned key", ErrVerify, b.Producer.InstallID, b.Producer.KeyID)
	}
	if pinnedKeyID != "" && pinnedKeyID != b.Producer.KeyID {
		return rep, fmt.Errorf("%w: bundle is signed by %s, not the pinned key %s",
			ErrVerify, b.Producer.KeyID, pinnedKeyID)
	}

	sigOK, err := verifyBundleSignature(signed, pub, b.Producer.KeyID)
	if err != nil {
		return rep, err
	}
	rep.SignatureOK = sigOK
	var head BundleCoord
	profile := ChainProfile
	if b.Chain != nil {
		head = b.Chain.Head
		if b.Chain.Profile != "" {
			profile = b.Chain.Profile
		}
	}
	// A sparse receipt is a different construction, not a broken linear one. Recomputing the linear
	// link over a tree claim always fails, so this command refused the output of its own
	// "receipt --sparse", which prints "Verify it with: switchtender verify".
	// Surfaces this product does not verify are refused by name, never silently skipped: a
	// disclosure or attestation the mirror ignored would ride inside a green verdict unchecked,
	// which is the exact rider problem the strip list exists to prevent. SwitchTender's own
	// receipts carry neither, so nothing this product emits is affected.
	if len(b.Attestations) > 0 {
		return rep, fmt.Errorf("%w: bundle carries attestations: this product does not verify"+
			" them; use the open loomseal verifier", ErrVerify)
	}
	for i := range b.Claims {
		if len(b.Claims[i].Disclosures) > 0 || len(b.Claims[i].Attestations) > 0 {
			return rep, fmt.Errorf("%w: claim %d carries disclosures or attestations: this"+
				" product does not verify them; use the open loomseal verifier", ErrVerify, i)
		}
		// A payload with a redactable-field digest set follows LoomSwatch rules this product
		// does not implement; passing it unexamined would grade a construction nobody checked.
		if _, ok := b.Claims[i].Payload["_sd"]; ok {
			return rep, fmt.Errorf("%w: claim %d carries redactable fields (_sd): this product"+
				" does not verify them; use the open loomseal verifier", ErrVerify, i)
		}
	}
	// The reference rule: a chain param naming an install must restate the producer, because a
	// param that binds nothing must not be settable to someone else and assumed to bind.
	if b.Chain != nil && b.Chain.Params["install_id"] != "" && b.Producer.InstallID != "" &&
		b.Chain.Params["install_id"] != b.Producer.InstallID {
		return rep, fmt.Errorf("%w: chain.params.install_id %s disagrees with producer install %s",
			ErrVerify, b.Chain.Params["install_id"], b.Producer.InstallID)
	}
	// A profile this product does not implement is named as unsupported, never fed to the linear
	// recompute: recomputing this profile's link over a foreign profile's claims always fails, so
	// an unknown profile used to read as a broken chain, which is the one wording it must never
	// wear. Fail closed, but say why.
	if profile != TreeProfile && profile != ChainProfile {
		return rep, fmt.Errorf("%w: chain profile %q: this product does not verify it; use the"+
			" open loomseal verifier", ErrVerify, profile)
	}
	if profile == TreeProfile {
		rep.ChainOK, rep.BrokeAtSeq = verifyBundleTree(&b)
	} else {
		// The linear chain is bound to its producer the same way the tree is. A link is a hash of the
		// entry's own fields and says nothing about who produced it, so a second install could
		// otherwise lift a published receipt whole, keep its claims and its genuine third-party
		// anchor, rewrite the producer block, and re-sign as itself.
		rep.ChainOK, rep.BrokeAtSeq = verifyBundleChain(b.Claims, head)
		if rep.ChainOK && !linearInstallMatches(&b) {
			rep.ChainOK, rep.BrokeAtSeq = false, head.Seq
		}
	}
	// An anchor is checked against the claim links, and for a tree those links are precisely what the
	// chain check validates. Reporting anchors satisfied over a chain that did not verify would let a
	// forged link poison both answers at once, so anchors are only meaningful once the chain holds.
	rep.AnchorsOK = rep.ChainOK && verifyBundleAnchors(&b)
	// A carried timestamp token is read, not taken on trust. A token that does not fix the link its
	// anchor names is a failure of the anchor, not a note beside it: the anchor's whole purpose is to be
	// the part of the record the producer cannot write.
	rep.TimestampsVerified, rep.TimestampProblems = verifyBundleProofs(&b)
	if len(rep.TimestampProblems) > 0 {
		rep.AnchorsOK = false
	}
	verifyOutcomeDisclosure(b.Claims, rep)
	verifyDecisionDisclosures(b.Claims, rep)
	verifySpecConsistency(b.Claims, rep)
	return rep, nil
}

// verifyDecisionDisclosures checks every disclosed approval decision against the digest its chain
// entry committed, and collects the verified ones so a reader learns who decided what. A receipt
// with no disclosed decisions leaves DecisionsOK true and is judged on its chain alone.
func verifyDecisionDisclosures(claims []BundleClaim, rep *BundleReport) {
	rep.DecisionsOK = true
	for _, c := range claims {
		method, _ := c.Payload["method"].(string)
		if method != MethodDecision {
			continue
		}
		bodyVal, hasBody := c.Payload["decision_body"]
		digest, _ := c.Payload["content_digest"].(string)
		if !hasBody || digest == "" {
			continue
		}
		rep.DecisionsPresent++
		nonce, _ := c.Payload["decision_nonce"].(string)
		body, err := json.Marshal(bodyVal)
		if err != nil || !VerifyContentDigest(digest, nonce, body) {
			rep.DecisionsOK = false
			continue
		}
		var rec struct {
			Verdict    string `json:"verdict"`
			SpecDigest string `json:"spec_digest"`
		}
		if err := json.Unmarshal(body, &rec); err != nil {
			rep.DecisionsOK = false
			continue
		}
		actor, _ := c.Payload["actor"].(string)
		actorType, _ := c.Payload["actor_type"].(string)
		rep.Decisions = append(rep.Decisions, DisclosedDecision{
			Actor: actor, ActorType: actorType, Verdict: rec.Verdict, SpecDigest: rec.SpecDigest,
		})
	}
}

// verifySpecConsistency ties the receipt's three statements about the run's spec to one another:
// the disclosed spec's recomputed digest, the spec digest the outcome record committed, and the
// digest each verified decision bound to. Every comparison that is possible must agree; a receipt
// missing one side simply has less to compare, which is reported through the Present fields rather
// than counted as a failure.
func verifySpecConsistency(claims []BundleClaim, rep *BundleReport) {
	rep.SpecConsistent = true
	var outcomeSpec string
	if rep.OutcomePresent && rep.OutcomeDigestOK {
		var rec struct {
			SpecDigest string `json:"spec_digest"`
		}
		if json.Unmarshal(rep.OutcomeBody, &rec) == nil {
			outcomeSpec = rec.SpecDigest
		}
	}
	var disclosed string
	for _, c := range claims {
		specVal, has := c.Payload["spec_body"]
		if !has {
			continue
		}
		// The spec is disclosed as the exact canonical bytes its digest was taken over. Reading it as
		// a tree and marshaling it again would have to reproduce those bytes exactly, which it cannot
		// for a number wider than a float, so a run with such a value in its extra vars read as
		// tampered when nothing had been touched.
		text, ok := specVal.(string)
		if !ok {
			rep.SpecConsistent = false
			return
		}
		body := []byte(text)
		rep.SpecPresent = true
		rep.SpecBody = body
		disclosed = UnkeyedDigestOf(body)
		break
	}
	if disclosed != "" && outcomeSpec != "" && disclosed != outcomeSpec {
		rep.SpecConsistent = false
	}
	for _, d := range rep.Decisions {
		if disclosed != "" && d.SpecDigest != disclosed {
			rep.SpecConsistent = false
		}
		if outcomeSpec != "" && d.SpecDigest != outcomeSpec {
			rep.SpecConsistent = false
		}
	}
}

// verifyOutcomeDisclosure checks a receipt that discloses a run's outcome. The outcome claim carries
// the outcome body and the nonce its digest was keyed under alongside the content_digest the chain
// commits. This confirms the disclosed body is exactly what the chain committed, so a verifier can
// trust what it reads about the run, not only that the chain is intact. A receipt without a
// disclosure leaves OutcomePresent false and is judged on its chain alone.
func verifyOutcomeDisclosure(claims []BundleClaim, rep *BundleReport) {
	for _, c := range claims {
		method, _ := c.Payload["method"].(string)
		path, _ := c.Payload["path"].(string)
		if method != MethodRun || !strings.Contains(path, "/outcome/") {
			continue
		}
		bodyVal, hasBody := c.Payload["outcome_body"]
		digest, _ := c.Payload["content_digest"].(string)
		if !hasBody || digest == "" {
			continue
		}
		nonce, _ := c.Payload["outcome_nonce"].(string)
		body, err := json.Marshal(bodyVal)
		if err != nil {
			return
		}
		rep.OutcomePresent = true
		rep.OutcomeBody = body
		rep.OutcomeDigestOK = VerifyContentDigest(digest, nonce, body)
		return
	}
}

// verifyBundleSignature reconstructs the exact bytes the producer signed: the canonical bundle
// with every unsigned surface stripped, which is the signatures array, head-level attestations,
// and each claim's disclosures and claim-level attestations. The strip list is FORMAT.md's, and
// the cross-verify gate runs this mirror against the reference corpus precisely because a mirror
// CAN disagree with the reference; the earlier wording here claimed it could not, while this
// function was quietly a release behind the list and refusing spec-valid bundles.
func verifyBundleSignature(signed []byte, pub ed25519.PublicKey, keyID string) (bool, error) {
	value, err := jcs.Parse(signed)
	if err != nil {
		return false, fmt.Errorf("%w: canonicalize bundle: %w", ErrVerify, err)
	}
	m, ok := value.(map[string]any)
	if !ok {
		return false, fmt.Errorf("%w: bundle is not a JSON object", ErrVerify)
	}
	sigs, ok := m["signatures"].([]any)
	if !ok || len(sigs) == 0 {
		return false, nil
	}
	// The producer's own signature, not whichever was written first. A bundle may carry several, and
	// taking the first lets anyone prepend one and decide which key gets checked.
	var sigB64 string
	for _, raw := range sigs {
		sigObj, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := sigObj["key_id"].(string); id == keyID {
			sigB64, _ = sigObj["sig"].(string)
			break
		}
	}
	if sigB64 == "" {
		return false, nil
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false, nil
	}
	m["signatures"] = []any{}
	delete(m, "attestations")
	if claims, ok := m["claims"].([]any); ok {
		for _, c := range claims {
			if obj, ok := c.(map[string]any); ok {
				delete(obj, "disclosures")
				delete(obj, "attestations")
			}
		}
	}
	canonical, err := jcs.Serialize(m)
	if err != nil {
		return false, fmt.Errorf("%w: canonicalize bundle: %w", ErrVerify, err)
	}
	return ed25519.Verify(pub, canonical, sig), nil
}

// verifyBundleTree checks a sparse receipt: every disclosed claim folds through its audit path to
// the tree head the bundle names.
//
// The install id the leaves are bound to is taken from the producer, never from the chain params a
// bundle carries alongside them. A copier who lifted somebody else's receipt and rewrote only the
// producer block would otherwise still fold, because the leaves would keep hashing under the
// original install. Requiring the two to agree is what ties the receipt to the install that signed
// it, and it is the rule the reference verifier applies.
func verifyBundleTree(b *Bundle) (bool, int64) {
	if b.Chain == nil {
		return false, 0
	}
	// A receipt that discloses nothing proves nothing, so it must not report that nothing was
	// altered. With no claims the fold below never runs and every check it performs is skipped, so
	// this returned true for any head at all: a document naming an arbitrary sequence and root,
	// signed by its author, read as VERIFIED backed by no entry, and the head it named was then
	// admissible as an anchor coordinate. The linear profile refuses an empty bundle for the same
	// reason, and this is the tree profile's half of that rule.
	if len(b.Claims) == 0 {
		return false, 0
	}
	installID := b.Producer.InstallID
	if installID == "" || b.Chain.Params["install_id"] != installID {
		return false, 0
	}
	root, err := hex.DecodeString(b.Chain.Head.Link)
	if err != nil || len(root) == 0 {
		return false, 0
	}
	size := b.Chain.Head.Seq
	var prevSeq int64
	for _, c := range b.Claims {
		if c.Inclusion == nil {
			return false, c.Chain.Seq
		}
		// A tree has no per-entry predecessor, and sequences must ascend, or a claim could be
		// presented twice or out of order to satisfy a proof built for a different position.
		if c.Chain.Prev != "" || c.Chain.Seq <= prevSeq {
			return false, c.Chain.Seq
		}
		prevSeq = c.Chain.Seq

		leafData, err := treeLeaf(c, installID)
		if err != nil {
			return false, c.Chain.Seq
		}
		// The declared link has to be the hash of the leaf the claim's content produces. Without
		// this the fold proved only that SOME leaf sits at the claimed position, never that it is
		// this claim's leaf, so a producer could declare any link, fold a matching path, and anchor
		// over it, and the receipt read as verified. This is the check the whole receipt rests on.
		if hex.EncodeToString(merkle.LeafHash(leafData)) != c.Chain.Link {
			return false, c.Chain.Seq
		}

		path, err := decodeProofHashes(c.Inclusion.Path)
		if err != nil {
			return false, c.Chain.Seq
		}
		// A claim's sequence is one based and the tree is zero based.
		if !merkle.VerifyInclusion(leafData, c.Chain.Seq-1, size, path, root) {
			return false, c.Chain.Seq
		}
	}
	// A carried consistency proof is verified, not merely displayed. Its from-root becomes an
	// admissible anchor coordinate below, and admitting a root the proof does not actually fold to
	// the head would let a producer pair a fabricated history with a genuine-looking anchor.
	if c := b.Chain.Consistency; c != nil {
		fromRoot, err := hex.DecodeString(c.FromRoot)
		if err != nil || len(fromRoot) == 0 {
			return false, 0
		}
		path, err := decodeProofHashes(c.Path)
		if err != nil {
			return false, 0
		}
		if !merkle.VerifyConsistency(c.FromSize, size, fromRoot, root, path) {
			return false, 0
		}
	}
	return true, 0
}

// decodeProofHashes turns a claim's hex audit path into the raw hashes the folder takes.
func decodeProofHashes(in []string) ([][]byte, error) {
	out := make([][]byte, 0, len(in))
	for _, h := range in {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// verifyBundleChain recomputes each claim's link and checks the claims chain to one another. The
// first claim carries whatever previous link the chain held at that point, since a bundle is often a
// window into a longer history rather than the genesis; every claim after it must name the previous
// claim's link as its own previous. It returns the sequence of the first claim that does not verify.
func verifyBundleChain(claims []BundleClaim, head BundleCoord) (bool, int64) {
	for i, c := range claims {
		// The genesis rule and contiguous ascending sequence numbers, the two checks the loomseal
		// reference verifier enforces that recomputing each link from the claim's own prev does not.
		// Without them a self-consistent chain with entries dropped between two it kept, or a window
		// opening past sequence one with an empty prev, recomputed cleanly and read as VERIFIED here
		// while the reference verifier the product tells relying parties to trust refused it.
		if i == 0 {
			if (c.Chain.Seq == 1) != (c.Chain.Prev == "") {
				return false, c.Chain.Seq
			}
		} else if c.Chain.Seq != claims[i-1].Chain.Seq+1 {
			return false, c.Chain.Seq
		}
		str := func(k string) string { s, _ := c.Payload[k].(string); return s }
		// The claim's values came from the document under test, not from this install, so a value
		// canonicalization refuses is a bad bundle rather than a bug here.
		recomputed, err := checkedLinkOf(claimObject(c.Chain.Seq, c.At,
			str("actor"), str("method"), str("path"), c.Chain.Prev,
			str("actor_type"), str("on_behalf_of"), str("content_digest"), str("install_id")))
		if err != nil || recomputed != c.Chain.Link {
			return false, c.Chain.Seq
		}
		if i > 0 && c.Chain.Prev != claims[i-1].Chain.Link {
			return false, c.Chain.Seq
		}
	}
	// The head is the value a reader quotes as where the log stood, and nothing checked it, so a
	// head naming a sequence and link the claims never produce verified as well as a true one.
	//
	// A head AHEAD of the newest claim is ordinary and stays allowed: a bundle is often a window into
	// a longer chain, and the head then attests a point this document cannot show. A head BEHIND the
	// newest claim is impossible, since the chain only grows. A head level with it must be it.
	if len(claims) > 0 {
		newest := claims[len(claims)-1].Chain
		if head.Seq < newest.Seq {
			return false, newest.Seq
		}
		if head.Seq == newest.Seq && head.Link != newest.Link {
			return false, newest.Seq
		}
		return true, 0
	}
	// A bundle carrying no claims proves nothing, so it must not report that nothing was altered.
	// The head is only constrained by the claims, so with none the loop above never ran and this
	// returned true for any subject and any head at all: a document naming a run that never happened,
	// at a sequence and link nobody produced, read as VERIFIED with only a parenthetical "(0 entries
	// recompute)" to give it away. A receipt is a claim about something; an empty one is not a true
	// claim about everything.
	if head.Seq != 0 || head.Link != "" {
		return false, head.Seq
	}
	return false, 0
}

// linearInstallMatches reports whether every claim that names an install names the signer's.
//
// A claim that names one is bound: the id is folded into its link, so a copier who rewrites the
// producer breaks this equality, and one who rewrites the claim's id too breaks the link. A claim
// that names none predates the binding and is left alone, which is what lets a chain spanning the
// upgrade verify rather than forcing a re-anchor. Those entries stay liftable and nothing here can
// change that: a link already written commits to what it committed to.
//
// A bundle from before the binding existed carries no install id at all. Those are refused rather
// than grandfathered: the whole value of the check is that a receipt names its origin, and an
// exception for documents that omit it is an exception any forger would take.
//
// This is a partial defense and it is worth being exact about what it does not do. Both values it
// compares are restatable, and the link preimage for this profile commits to neither, so a copier
// who rewrites the producer block and the chain params together passes it with every link still
// recomputing. It catches the careless case and costs nothing. The complete fix is to hash the
// install into the link, as the format's other two profiles do, which is a change to the shared
// format and the reference verifier rather than to this file alone.
func linearInstallMatches(b *Bundle) bool {
	for i := range b.Claims {
		named, _ := b.Claims[i].Payload["install_id"].(string)
		if named == "" {
			// An entry written before the binding existed. It hashes as it always did and is not
			// bound, which is a fact about that entry rather than a fault in this document.
			continue
		}
		if b.Producer.InstallID == "" || named != b.Producer.InstallID {
			return false
		}
	}
	return true
}

// verifyBundleAnchors reports whether every anchor names a coordinate the bundle actually holds at
// the anchored value. An anchor for a coordinate the bundle does not carry is the tampered export
// the reference verifier rejects, so it fails here too. It runs only after the chain check passed,
// which is what decides which coordinates are admissible.
func verifyBundleAnchors(b *Bundle) bool {
	// For the linear profile the map is built from the claims alone. Seeding it from the declared
	// head let an anchor over a forged head check against the forgery, which turned the anchor into
	// a second copy of the producer's own claim rather than independent evidence about it. The head
	// is admissible only once the chain check has confirmed it is the newest claim, at which point
	// it is already in this map by way of that claim.
	links := make(map[int64]string, len(b.Claims)+2)
	for _, c := range b.Claims {
		links[c.Chain.Seq] = c.Chain.Link
	}
	// For the tree profile the head and the consistency from-root are admissible, because by now
	// the chain check has proved them: every disclosed claim folded through its audit path to the
	// head root, and the consistency proof folded the from-root to it. A root-anchored sparse
	// receipt is the shape an anchored install actually emits, and holding its anchor against the
	// leaf hashes alone refused every honest one.
	if b.Chain != nil && b.Chain.Profile == TreeProfile {
		links[b.Chain.Head.Seq] = b.Chain.Head.Link
		if c := b.Chain.Consistency; c != nil {
			links[c.FromSize] = c.FromRoot
		}
	}
	for _, a := range b.Anchors {
		if link, ok := links[a.Seq]; !ok || link != a.Link {
			return false
		}
	}
	return true
}

// verifyBundleProofs checks every embedded timestamp token against the link its anchor names, and
// reports how many verified and what was wrong with the rest.
//
// A carried token used to be described rather than checked: an anchor reported as satisfied because the
// chain reached its link, and a proof string reported as an offline proof because it was present. The
// authority's own statement, which is the only part of an anchor that does not come from the producer,
// went unread by every verifier.
func verifyBundleProofs(b *Bundle) (int, []string) {
	var verified int
	var problems []string
	// Claim times by seq, for the backdate rule: a token commits to a link that hashes a claim
	// carrying its own time, so an authority cannot honestly have signed it earlier. Both clocks
	// are real and neither is authoritative, so a few minutes of skew is allowed and a backdated
	// month is not, the same allowance the reference verifier applies.
	const anchorClockSkew = 5 * time.Minute
	claimAt := make(map[int64]time.Time, len(b.Claims))
	for i := range b.Claims {
		if at, err := time.Parse(time.RFC3339, b.Claims[i].At); err == nil {
			claimAt[b.Claims[i].Chain.Seq] = at
		}
	}
	for _, a := range b.Anchors {
		if a.Type != AnchorRFC3161 || a.Proof == "" {
			continue
		}
		genTime, err := VerifyTimestampProofTime(a.Link, a.Proof)
		if err != nil {
			problems = append(problems, fmt.Sprintf("anchor at %d: %v", a.Seq, err))
			continue
		}
		if at, ok := claimAt[a.Seq]; ok && !genTime.IsZero() &&
			genTime.Before(at.Add(-anchorClockSkew)) {
			problems = append(problems, fmt.Sprintf(
				"anchor at %d attests %s over an entry the bundle says happened at %s: a timestamp"+
					" cannot precede the entry it covers", a.Seq,
				genTime.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339)))
			continue
		}
		verified++
	}
	return verified, problems
}
