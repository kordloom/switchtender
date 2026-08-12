package audit

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kordloom/loomseal/jcs"
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
}

// OK reports whether every check passed, the single question a verify command answers yes or no. A
// disclosed outcome that does not match its commitment fails the whole receipt: it is a claim about
// what the run did that the chain does not back.
func (r *BundleReport) OK() bool {
	return r.SignatureOK && r.ChainOK && r.AnchorsOK && (!r.OutcomePresent || r.OutcomeDigestOK)
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
	rep.ChainOK, rep.BrokeAtSeq = verifyBundleChain(b.Claims)
	rep.AnchorsOK = verifyBundleAnchors(&b)
	verifyOutcomeDisclosure(b.Claims, rep)
	return rep, nil
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

// verifyBundleChain recomputes each claim's link and checks the claims chain to one another. The
// first claim carries whatever previous link the chain held at that point, since a bundle is often a
// window into a longer history rather than the genesis; every claim after it must name the previous
// claim's link as its own previous. It returns the sequence of the first claim that does not verify.
func verifyBundleChain(claims []BundleClaim) (bool, int64) {
	for i, c := range claims {
		str := func(k string) string { s, _ := c.Payload[k].(string); return s }
		recomputed := linkOf(claimObject(c.Chain.Seq, c.At,
			str("actor"), str("method"), str("path"), c.Chain.Prev,
			str("actor_type"), str("on_behalf_of"), str("content_digest")))
		if recomputed != c.Chain.Link {
			return false, c.Chain.Seq
		}
		if i > 0 && c.Chain.Prev != claims[i-1].Chain.Link {
			return false, c.Chain.Seq
		}
	}
	return true, 0
}

// verifyBundleAnchors reports whether every anchor names a claim, or the head, the bundle actually
// holds at the anchored link. An anchor for a sequence the bundle does not carry is the tampered
// export the reference verifier rejects, so it fails here too.
func verifyBundleAnchors(b *Bundle) bool {
	links := make(map[int64]string, len(b.Claims)+1)
	for _, c := range b.Claims {
		links[c.Chain.Seq] = c.Chain.Link
	}
	if b.Chain != nil {
		links[b.Chain.Head.Seq] = b.Chain.Head.Link
	}
	for _, a := range b.Anchors {
		if link, ok := links[a.Seq]; !ok || link != a.Link {
			return false
		}
	}
	return true
}
