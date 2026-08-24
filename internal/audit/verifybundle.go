package audit

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/merkle"
	"github.com/kordloom/loomseal/seal"
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
	if profile == TreeProfile {
		rep.ChainOK, rep.BrokeAtSeq = verifyBundleTree(&b)
	} else {
		rep.ChainOK, rep.BrokeAtSeq = verifyBundleChain(b.Claims, head)
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

// verifyBundleSignature reconstructs the exact bytes the producer signed, the canonical bundle with
// its signatures emptied, and checks the ed25519 signature over them. It mirrors seal.SignBundle so
// the two cannot disagree about what a signature covers.
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
		str := func(k string) string { s, _ := c.Payload[k].(string); return s }
		// The claim's values came from the document under test, not from this install, so a value
		// canonicalization refuses is a bad bundle rather than a bug here.
		recomputed, err := checkedLinkOf(claimObject(c.Chain.Seq, c.At,
			str("actor"), str("method"), str("path"), c.Chain.Prev,
			str("actor_type"), str("on_behalf_of"), str("content_digest")))
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
	}
	return true, 0
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
	for _, a := range b.Anchors {
		if a.Type != AnchorRFC3161 || a.Proof == "" {
			continue
		}
		if err := VerifyTimestampProof(a.Link, a.Proof); err != nil {
			problems = append(problems, fmt.Sprintf("anchor at %d: %v", a.Seq, err))
			continue
		}
		verified++
	}
	return verified, problems
}
