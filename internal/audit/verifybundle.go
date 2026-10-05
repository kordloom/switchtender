package audit

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/merkle"
	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/decision"
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
	// BrokeAtSeq is the sequence of the claim the chain check stopped at, zero when the chain is whole
	// or the fault has no position of its own.
	BrokeAtSeq int64
	// ChainProblem says what stopped the chain check, naming the position, empty when the chain is
	// whole. It is not always that claim's own link: the claim after a gap recomputes, and what is
	// wrong is the entries missing before it, so a gap names the sequence numbers it leaves out.
	ChainProblem string
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
	// TimeProblems names each place a claim's recorded time does not advance past the claim before
	// it. A written link commits to the time it holds and cannot be repaired, so this is reported
	// rather than treated as a broken chain, and a reader decides what a clock that went backward
	// means for the record in front of them.
	TimeProblems []string
	// ApprovalPrecedesRun reports that every approval a receipt discloses was recorded before the
	// outcome it released. False means the record shows a run executing before it was approved,
	// which is the gate not holding, and it fails the receipt.
	ApprovalPrecedesRun bool
	// CorrectionsPresent counts the corrections to a decision's reason the receipt disclosed.
	CorrectionsPresent int
	// CorrectionsFailed reports a disclosed correction that does not match the digest its chain entry
	// committed or whose text does not open its commitment. It is a failure flag rather than an OK
	// flag so a report that discloses no corrections, or one built before corrections existed, reads
	// as having nothing wrong with them.
	CorrectionsFailed bool
	// Corrections are the verified corrections.
	Corrections []DisclosedCorrection
	// SpansUnbound reports that a span beat's members disagree with the path its link committed. It
	// is a failure flag, so a document with no span beats reads as having nothing wrong with them.
	SpansUnbound bool
	// CaseVariants lists, as "claim N member", each payload member whose name differs only in case
	// from a member a check reads on that claim. A reader folding case would take it for the member
	// that was checked, so any entry fails the document.
	CaseVariants []string
	// LegacyRecords lists, as "claim N member", each disclosed record verified against the legacy
	// unkeyed digest form from before nonces. It verifies, and anyone who can guess its body can
	// confirm the guess against the digest, which is why it is named.
	LegacyRecords []string
	// LegacyAfterKeyed lists, as "claim N member", each disclosed record under the unkeyed form on an
	// entry after the first keyed one. This product keys every entry from then on, so such a record
	// is not one from before nonces, and any entry fails the document.
	LegacyAfterKeyed []string
	// Disclosed lists every payload member the chain link does not commit, each checked against a
	// commitment, redacted, or unchecked, with what it was held against or why not.
	Disclosed []DisclosedMember
	// DisclosedUnchecked is how many disclosed records are unchecked, a member and the one it travels
	// with counted once. A document with any never verifies as plain VERIFIED.
	DisclosedUnchecked int
	// memberStates holds what each check established about the members it covered.
	memberStates map[int]map[string]memberSettled
}

// Reason states, for a decision or a correction whose body commits a reason.
const (
	// ReasonVerified is a disclosed reason whose text and random value open the commitment.
	ReasonVerified = "verified"
	// ReasonRedacted is a reason removed by a redaction, whose commitment can no longer be opened.
	ReasonRedacted = "redacted"
	// ReasonWithheld is a reason the chain committed that the document does not disclose.
	ReasonWithheld = "withheld"
)

// DisclosedCorrection is one correction to a decision's reason read back from a receipt.
type DisclosedCorrection struct {
	// Actor is who wrote it, as the chain committed it.
	Actor string
	// DecisionID is the decision it corrects.
	DecisionID string
	// Reason is the correction's text, when disclosed and verified.
	Reason string
	// ReasonState is verified, redacted, or withheld.
	ReasonState string
	// RedactedCategory is the redaction's category when the text was redacted.
	RedactedCategory string
}

// DisclosedDecision is one digest-verified approval decision read back from a receipt.
type DisclosedDecision struct {
	// Actor is who decided, as the chain committed it.
	Actor string
	// ActorType is how the decider authenticated.
	ActorType string
	// OnBehalfOf is the account whose authority the decider used, as the chain committed it. It is
	// empty when the decider acted as itself and on a decision recorded before decisions carried it.
	OnBehalfOf string
	// Verdict is approved or rejected.
	Verdict string
	// SpecDigest is the digest of the spec the decision bound to.
	SpecDigest string
	// DecisionID is the decision record's id the body commits, empty on a decision recorded before
	// decision records existed.
	DecisionID string
	// Reason is the approver's reason, set when it was disclosed and opens the commitment the body
	// carries.
	Reason string
	// ReasonState is verified, redacted, or withheld for a decision whose body commits a reason, and
	// empty for one given no reason.
	ReasonState string
	// RedactedCategory is the redaction's category when the reason was redacted.
	RedactedCategory string
	// SeparationOfDuties is the evaluation the body commits for a decision on an agent's run.
	SeparationOfDuties *decision.SeparationOfDuties
}

// OK reports whether every check passed, the single question a verify command answers yes or no. A
// disclosed outcome or decision that does not match its commitment fails the whole receipt: it is a
// claim the chain does not back. A spec inconsistency fails it too, because then the approval and
// the execution the receipt ties together are not about the same change.
func (r *BundleReport) OK() bool {
	return r.SignatureOK && r.ChainOK && r.AnchorsOK &&
		(!r.OutcomePresent || r.OutcomeDigestOK) && r.DecisionsOK && r.SpecConsistent &&
		r.ApprovalPrecedesRun && !r.CorrectionsFailed && !r.SpansUnbound &&
		len(r.CaseVariants) == 0 && len(r.LegacyAfterKeyed) == 0
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
	// Refuse a case variant of any known member before a verdict reads a struct field decoded from it.
	// The struct decode above matches members case-insensitively, so without this a claim carrying
	// both at and At, or a producer carrying install_id and Install_ID, would fold the variant into
	// the field this verifier reads while a reader and the leaf saw the exact member.
	if err := checkExactMembers(signed); err != nil {
		return nil, err
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
	// The id a bundle was issued under is accepted at the width it was issued under. The derivation
	// was widened so an install id could not be ground out, and a receipt keeps verifying for as
	// long as somebody holds it, so reading one issued before that change must not report it as a
	// rotated install. New installs are minted at the full width either way.
	if b.Producer.InstallID != "" && b.Producer.InstallID != acceptedInstall &&
		b.Producer.InstallID != identity.InstallIDFromKey(pub) &&
		b.Producer.InstallID != identity.LegacyInstallIDFromKey(pub) {
		return rep, fmt.Errorf("%w: bundle names install %s, which is not the install key %s was born"+
			" to; a rotated install verifies only with its install id explicitly accepted for the"+
			" pinned key", ErrVerify, b.Producer.InstallID, b.Producer.KeyID)
	}
	if pinnedKeyID != "" && pinnedKeyID != b.Producer.KeyID {
		return rep, fmt.Errorf("%w: bundle is signed by %s, not the pinned key %s",
			ErrVerify, b.Producer.KeyID, pinnedKeyID)
	}
	// The names this key may have written entries under. Accepting the widened id for the producer
	// block alone repaired the header and left every entry beneath it failing, so a chain spanning
	// the upgrade still read as tampered.
	names := producerNames(pub, b.Producer.InstallID)

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
		rep.ChainOK, rep.BrokeAtSeq, rep.ChainProblem = verifyBundleTree(&b)
	} else {
		// The linear chain is bound to its producer the same way the tree is. A link is a hash of the
		// entry's own fields and says nothing about who produced it, so a second install could
		// otherwise lift a published receipt whole, keep its claims and its genuine third-party
		// anchor, rewrite the producer block, and re-sign as itself.
		rep.ChainOK, rep.BrokeAtSeq, rep.ChainProblem = verifyBundleChain(b.Claims, head)
		if rep.ChainOK {
			if matches, seq := linearInstallMatches(&b, names); !matches {
				rep.ChainOK, rep.BrokeAtSeq = false, seq
				rep.ChainProblem = fmt.Sprintf("the entry at seq %d names an install the signing "+
					"key does not speak for", seq)
			}
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
	verifyCorrectionDisclosures(b.Claims, rep)
	verifySpecConsistency(b.Claims, rep)
	verifySpanBinding(b.Claims, rep)
	verifyTimeOrder(b.Claims, rep)
	classifyDisclosed(&b, rep)
	return rep, nil
}

// verifyTimeOrder reads the times the chain committed. Two different questions live here.
//
// The first is whether time advances across the chain at all. It usually should, and when it does
// not the record is worth a second look, but a written link commits to the time it holds and cannot
// be corrected afterward, so a clock that stepped backward is reported and left for the reader
// rather than treated as tampering.
//
// The second is the one this product exists to answer: an approval must be recorded before the run
// it released. A receipt showing a run that executed and was approved twenty minutes later is a
// receipt showing the gate being bypassed, and until this check existed the verifier printed
// VERIFIED over exactly that. Digests agreeing is not enough, because the same spec can be approved
// after the fact.
func verifyTimeOrder(claims []BundleClaim, rep *BundleReport) {
	rep.ApprovalPrecedesRun = true
	ordered := make([]BundleClaim, len(claims))
	copy(ordered, claims)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Chain.Seq < ordered[j].Chain.Seq
	})

	// Approvals and outcomes are matched by the run they name, not by their order in the chain.
	// Scoping this to "an approval seen earlier in the sequence" was the first version of this check
	// and it missed the same defect written the other way round: a chain that records the outcome
	// first and the approval second passed, because no approval had been seen yet when the outcome
	// was read.
	approved := make(map[string]time.Time)
	type ranAt struct {
		run string
		at  time.Time
		seq int64
	}
	var runs []ranAt

	var prevAt time.Time
	var prevSeq int64
	for _, c := range ordered {
		at, err := time.Parse(time.RFC3339Nano, c.At)
		if err != nil {
			rep.TimeProblems = append(rep.TimeProblems,
				fmt.Sprintf("claim %d carries an unreadable time %q", c.Chain.Seq, c.At))
			continue
		}
		if !prevAt.IsZero() && at.Before(prevAt) {
			rep.TimeProblems = append(rep.TimeProblems, fmt.Sprintf(
				"claim %d is dated %s, before claim %d at %s",
				c.Chain.Seq, at.UTC().Format(time.RFC3339), prevSeq, prevAt.UTC().Format(time.RFC3339)))
		}
		prevAt, prevSeq = at, c.Chain.Seq

		method, _ := c.Payload["method"].(string)
		path, _ := c.Payload["path"].(string)
		id := runIDFromPath(path)
		switch {
		case method == MethodDecision && strings.Contains(path, "/decision/approved"):
			if prev, seen := approved[id]; !seen || at.After(prev) {
				approved[id] = at
			}
		case method == MethodRun && strings.Contains(path, "/outcome/"):
			runs = append(runs, ranAt{run: id, at: at, seq: c.Chain.Seq})
		}
	}

	for _, r := range runs {
		ap, seen := approved[r.run]
		if !seen || !ap.After(r.at) {
			continue
		}
		rep.ApprovalPrecedesRun = false
		rep.TimeProblems = append(rep.TimeProblems, fmt.Sprintf(
			"the run recorded at %s was approved at %s, after it ran",
			r.at.UTC().Format(time.RFC3339), ap.UTC().Format(time.RFC3339)))
	}
}

// runIDFromPath returns the run a claim's path names, so an approval and an outcome are matched by
// the run they belong to rather than by where they sit in the chain. An empty string groups every
// claim whose path names no run, which is correct: they are compared only against each other.
func runIDFromPath(path string) string {
	const marker = "/runs/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return rest
}

// verifyDecisionDisclosures checks every disclosed approval decision against the digest its chain
// entry committed, and collects the verified ones so a reader learns who decided what. A receipt
// with no disclosed decisions leaves DecisionsOK true and is judged on its chain alone.
func verifyDecisionDisclosures(claims []BundleClaim, rep *BundleReport) {
	rep.DecisionsOK = true
	keyedAt := firstKeyed(claims)
	for i, c := range claims {
		if recordKindOf(c.Payload) != recordDecision {
			continue
		}
		if !recordFormHolds(i, c.Payload, recordDecision, "decision_body", keyedAt, rep) {
			continue
		}
		bodyVal, hasBody := c.Payload["decision_body"]
		digest, _ := c.Payload["content_digest"].(string)
		if !hasBody || digest == "" {
			continue
		}
		rep.DecisionsPresent++
		nonce, _ := c.Payload["decision_nonce"].(string)
		if !disclosedRecordVerifies(digest, nonce, bodyVal) {
			rep.DecisionsOK = false
			continue
		}
		// The digest above commits the whole body. Its fields are then read from the exact canonical
		// members of the body object, never from a case-insensitive struct decode of the re-marshaled
		// bytes, so a case variant such as Spec_Digest beside spec_digest cannot be read in place of
		// the member the digest committed and a reader sees.
		bodyMap, ok := bodyVal.(map[string]any)
		if !ok {
			rep.DecisionsOK = false
			continue
		}
		var rec struct {
			Verdict            string
			SpecDigest         string
			DecisionID         string
			ReasonCommitment   string
			SeparationOfDuties *decision.SeparationOfDuties
		}
		rec.Verdict, _ = bodyMap["verdict"].(string)
		rec.SpecDigest, _ = bodyMap["spec_digest"].(string)
		rec.DecisionID, _ = bodyMap["decision_id"].(string)
		rec.ReasonCommitment, _ = bodyMap["reason_commitment"].(string)
		rec.SeparationOfDuties = separationOfDuties(bodyMap["separation_of_duties"])
		rep.settleRecord(i, digest, "decision_body", "decision_nonce")
		actor, _ := c.Payload["actor"].(string)
		actorType, _ := c.Payload["actor_type"].(string)
		onBehalfOf, _ := c.Payload["on_behalf_of"].(string)
		d := DisclosedDecision{
			Actor: actor, ActorType: actorType, OnBehalfOf: onBehalfOf, Verdict: rec.Verdict,
			SpecDigest: rec.SpecDigest, DecisionID: rec.DecisionID,
			SeparationOfDuties: rec.SeparationOfDuties,
		}
		state, text, category, ok := checkReason(c.Payload, rec.ReasonCommitment, rec.DecisionID)
		if !ok {
			rep.DecisionsOK = false
		} else {
			rep.settleReason(i, state)
		}
		d.ReasonState, d.Reason, d.RedactedCategory = state, text, category
		rep.Decisions = append(rep.Decisions, d)
	}
}

// disclosedRecordVerifies checks a disclosed decision or correction body against the digest its
// entry committed. The canonical bytes of the body as disclosed are checked first, with nothing
// reduced, which is the check every LoomSeal verifier makes. A receipt issued before bodies were
// disclosed in their canonical redacted form carried the body as it was assembled, which matches
// only after this product's redaction, so that is the fallback. The body's fields are read by the
// caller from its exact members, never from a decode of these bytes.
func disclosedRecordVerifies(digest, nonce string, bodyVal any) bool {
	body, err := json.Marshal(bodyVal)
	if err != nil {
		return false
	}
	if canonical, cerr := jcs.Canonicalize(body); cerr == nil &&
		VerifyCanonicalDigest(digest, nonce, canonical) {
		return true
	}
	return VerifyContentDigest(digest, nonce, body)
}

// recordFormHolds checks what a record claim must hold before its record is read: no member whose
// name differs only in case from one the record is read for, and no record under the unkeyed digest
// form after the first keyed entry. It records a failure and reports false when either is broken.
func recordFormHolds(i int, payload map[string]any, kind, body string, keyedAt int,
	rep *BundleReport) bool {
	if variant, found := caseVariant(payload, recordMembers(kind)); found {
		rep.CaseVariants = append(rep.CaseVariants, fmt.Sprintf("claim %d %s", i, variant))
		return false
	}
	digest, _ := payload["content_digest"].(string)
	if _, disclosed := payload[body]; disclosed && isUnkeyed(digest) && keyedAt >= 0 && keyedAt < i {
		rep.LegacyAfterKeyed = append(rep.LegacyAfterKeyed, fmt.Sprintf("claim %d %s", i, body))
		return false
	}
	return true
}

// settleRecord records a verified record's members as checked, against the legacy unkeyed form when
// that is the form its entry committed, which is also listed so a reader sees the digest confirms a
// guess.
func (r *BundleReport) settleRecord(claim int, digest string, members ...string) {
	if !isUnkeyed(digest) {
		r.settleChecked(claim, members...)
		return
	}
	r.LegacyRecords = append(r.LegacyRecords, fmt.Sprintf("claim %d %s", claim, members[0]))
	for _, m := range members {
		r.settle(claim, m, MemberChecked, legacyDetail)
	}
}

// separationOfDuties decodes the separation-of-duties evaluation a decision body commits, from the
// exact member the body carries. It is a display field on the disclosed decision, never compared for
// a verdict, and is re-marshaled from the committed member rather than read field by field because its
// shape is owned elsewhere. A nil or non-object member yields nil.
func separationOfDuties(v any) *decision.SeparationOfDuties {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	var sod decision.SeparationOfDuties
	if json.Unmarshal(raw, &sod) != nil {
		return nil
	}
	return &sod
}

// checkReason reads the reason a decision or correction claim discloses against the commitment its
// verified body carries. It reports the reason's state, its text when verified, the redaction's
// category, and false when the claim discloses something the chain does not back: text that does
// not open the commitment, or a reason where the body commits none.
func checkReason(payload map[string]any, commitment, eventID string) (state, text, category string,
	ok bool) {
	text, hasText := payload["reason_text"].(string)
	random, _ := payload["reason_random"].(string)
	category, redacted := payload["reason_redacted"].(string)
	if commitment == "" {
		// A reason disclosed beside a body that commits none is text the chain never fixed, and a
		// reader must not be shown it as part of the record.
		return "", "", "", !hasText && !redacted
	}
	switch {
	case hasText:
		if !decision.Verify(commitment, eventID, random, text) {
			return "", "", "", false
		}
		return ReasonVerified, text, "", true
	case redacted:
		return ReasonRedacted, "", category, true
	default:
		return ReasonWithheld, "", "", true
	}
}

// verifyCorrectionDisclosures checks every disclosed correction to a decision's reason the way a
// decision is checked: the body against the digest its chain entry committed, and the text against
// the commitment the body carries. A receipt with no disclosed corrections fails nothing here.
func verifyCorrectionDisclosures(claims []BundleClaim, rep *BundleReport) {
	keyedAt := firstKeyed(claims)
	for i, c := range claims {
		if recordKindOf(c.Payload) != recordCorrection {
			continue
		}
		if !recordFormHolds(i, c.Payload, recordCorrection, "correction_body", keyedAt, rep) {
			continue
		}
		bodyVal, hasBody := c.Payload["correction_body"]
		digest, _ := c.Payload["content_digest"].(string)
		if !hasBody || digest == "" {
			continue
		}
		rep.CorrectionsPresent++
		nonce, _ := c.Payload["correction_nonce"].(string)
		if !disclosedRecordVerifies(digest, nonce, bodyVal) {
			rep.CorrectionsFailed = true
			continue
		}
		// The digest commits the whole body; its fields are read from the exact canonical members,
		// not a case-insensitive struct decode, for the reason the decision check states.
		bodyMap, ok := bodyVal.(map[string]any)
		if !ok {
			rep.CorrectionsFailed = true
			continue
		}
		var rec struct {
			DecisionID       string
			CorrectionID     string
			ReasonCommitment string
		}
		rec.DecisionID, _ = bodyMap["decision_id"].(string)
		rec.CorrectionID, _ = bodyMap["correction_id"].(string)
		rec.ReasonCommitment, _ = bodyMap["reason_commitment"].(string)
		if rec.ReasonCommitment == "" {
			rep.CorrectionsFailed = true
			continue
		}
		state, text, category, ok := checkReason(c.Payload, rec.ReasonCommitment, rec.CorrectionID)
		if !ok {
			rep.CorrectionsFailed = true
			continue
		}
		rep.settleRecord(i, digest, "correction_body", "correction_nonce")
		rep.settleReason(i, state)
		actor, _ := c.Payload["actor"].(string)
		rep.Corrections = append(rep.Corrections, DisclosedCorrection{Actor: actor,
			DecisionID: rec.DecisionID, Reason: text, ReasonState: state,
			RedactedCategory: category})
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
		// The outcome body is the exact bytes the producer ordered and the digest committed, so its
		// spec digest is read from the exact canonical member, never a case-insensitive struct decode.
		// Folding a Spec_Digest the producer placed after spec_digest would let the spec-consistency
		// check compare a value no reader sees against the decisions and the disclosed spec.
		var m map[string]any
		if json.Unmarshal(rep.OutcomeBody, &m) == nil {
			outcomeSpec, _ = m["spec_digest"].(string)
		}
	}
	// Every disclosed spec is compared, not the first. Breaking at the first one let a second,
	// different spec_body sit in the same document unexamined while the report called the document
	// consistent: two disclosures that disagree about what was approved is exactly the state this
	// check exists to catch.
	var disclosed string
	var specClaims []int
	for i, c := range claims {
		// A spec is read only on the outcome claim, where this product discloses it. The same name on
		// any other claim is an ordinary member, which nothing commits.
		if recordKindOf(c.Payload) != recordOutcome {
			continue
		}
		specVal, has := c.Payload["spec_body"]
		if !has {
			continue
		}
		specClaims = append(specClaims, i)
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
		// Hashed exactly as disclosed, the same as the producer stamps it. UnkeyedDigestOf would
		// reduce these already-reduced bytes a second time, which is not idempotent, so a genuine
		// spec could hash to a value the chain never committed while a tampered one collided with
		// the value it did.
		got := UnkeyedDigestOfReduced(body)
		if !rep.SpecPresent {
			rep.SpecPresent = true
			rep.SpecBody = body
			disclosed = got
			continue
		}
		if got != disclosed {
			rep.SpecConsistent = false
		}
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
	// A disclosed spec is checked only when something verified named a digest to hold it against.
	compared := outcomeSpec != "" || len(rep.Decisions) > 0
	for _, i := range specClaims {
		switch {
		case rep.SpecConsistent && compared:
			rep.settleChecked(i, "spec_body")
		case !compared:
			rep.settle(i, "spec_body", MemberUnchecked, "nothing verified names a spec digest")
		}
	}
}

// verifyOutcomeDisclosure checks a receipt that discloses a run's outcome. The outcome claim carries
// the outcome body and the nonce its digest was keyed under alongside the content_digest the chain
// commits. This confirms the disclosed body is exactly what the chain committed, so a verifier can
// trust what it reads about the run, not only that the chain is intact. A receipt without a
// disclosure leaves OutcomePresent false and is judged on its chain alone.
func verifyOutcomeDisclosure(claims []BundleClaim, rep *BundleReport) {
	// Every disclosed outcome is checked, not the first one.
	//
	// Returning at the first claim carrying a body meant a document disclosing two outcomes was
	// judged on one of them: the first genuine, the second fabricated and not matching the digest
	// its own chain entry committed, and the report still said the disclosed outcome matches what
	// the chain committed, under a VERIFIED verdict. OutcomeDigestOK is an assertion about what the
	// reader is being shown, so it has to hold for all of it. The decision disclosures next door
	// already work this way.
	keyedAt := firstKeyed(claims)
	for i, c := range claims {
		if recordKindOf(c.Payload) != recordOutcome {
			continue
		}
		if !recordFormHolds(i, c.Payload, recordOutcome, "outcome_body", keyedAt, rep) {
			continue
		}
		bodyVal, hasBody := c.Payload["outcome_body"]
		digest, _ := c.Payload["content_digest"].(string)
		if !hasBody || digest == "" {
			continue
		}
		nonce, _ := c.Payload["outcome_nonce"].(string)
		// Read as the exact bytes disclosed, the same as the spec. Marshaling a re-parsed tree back
		// to bytes cannot reproduce what the digest committed, above the size cap where key order
		// diverges from field order and at any size for a number wider than a float, so an honest
		// receipt for a large outcome read as tampered.
		text, isText := bodyVal.(string)
		if !isText {
			// A disclosure that is not the exact-bytes form cannot be checked, which is a failure
			// rather than a reason to stop looking.
			rep.OutcomePresent = true
			rep.OutcomeDigestOK = false
			continue
		}
		body := []byte(text)
		ok := VerifyContentDigest(digest, nonce, body)
		if ok {
			rep.settleRecord(i, digest, "outcome_body", "outcome_nonce")
		}
		if !rep.OutcomePresent {
			// The first disclosure is the one the report shows, so the body a reader sees is
			// unchanged; what changes is that a later bad one can no longer be vouched for.
			rep.OutcomePresent = true
			rep.OutcomeBody = body
			rep.OutcomeDigestOK = ok
			continue
		}
		if !ok {
			rep.OutcomeDigestOK = false
		}
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
// the tree head the bundle names. It returns the sequence of the claim the check stopped at, zero
// when the fault belongs to no one claim, and what was wrong.
//
// The install id the leaves are bound to is taken from the producer, never from the chain params a
// bundle carries alongside them. A copier who lifted somebody else's receipt and rewrote only the
// producer block would otherwise still fold, because the leaves would keep hashing under the
// original install. Requiring the two to agree is what ties the receipt to the install that signed
// it, and it is the rule the reference verifier applies.
func verifyBundleTree(b *Bundle) (ok bool, brokeAt int64, problem string) {
	if b.Chain == nil {
		return false, 0, "no chain head is declared, so there is no root to fold to"
	}
	// A receipt that discloses nothing proves nothing, so it must not report that nothing was
	// altered. With no claims the fold below never runs and every check it performs is skipped, so
	// this returned true for any head at all: a document naming an arbitrary sequence and root,
	// signed by its author, read as VERIFIED backed by no entry, and the head it named was then
	// admissible as an anchor coordinate. The linear profile refuses an empty bundle for the same
	// reason, and this is the tree profile's half of that rule.
	if len(b.Claims) == 0 {
		return false, 0, "no entries are carried, so there is nothing to recompute"
	}
	installID := b.Producer.InstallID
	if installID == "" {
		return false, 0, "the producer names no install for the leaves to bind to"
	}
	if b.Chain.Params["install_id"] != installID {
		return false, 0, "the chain does not bind its leaves to the producer's install"
	}
	root, err := hex.DecodeString(b.Chain.Head.Link)
	if err != nil || len(root) == 0 {
		return false, 0, "the head names a root that is not a hash"
	}
	size := b.Chain.Head.Seq
	var prevSeq int64
	for _, c := range b.Claims {
		if c.Inclusion == nil {
			return false, c.Chain.Seq, fmt.Sprintf("the entry at seq %d carries no audit path",
				c.Chain.Seq)
		}
		// A tree has no per-entry predecessor, and sequences must ascend, or a claim could be
		// presented twice or out of order to satisfy a proof built for a different position.
		if c.Chain.Prev != "" {
			return false, c.Chain.Seq, fmt.Sprintf("the entry at seq %d carries a previous link, "+
				"which a tree entry cannot have", c.Chain.Seq)
		}
		if c.Chain.Seq < 1 {
			return false, c.Chain.Seq, fmt.Sprintf("the entry at seq %d names no position in a log, "+
				"which starts at seq 1", c.Chain.Seq)
		}
		if c.Chain.Seq <= prevSeq {
			return false, c.Chain.Seq, sequenceProblem(b.Claims, prevSeq, c.Chain.Seq)
		}
		prevSeq = c.Chain.Seq

		leafData, err := treeLeafFor(c, installID)
		// The declared link has to be the hash of the leaf the claim's content produces. Without
		// this the fold proved only that SOME leaf sits at the claimed position, never that it is
		// this claim's leaf, so a producer could declare any link, fold a matching path, and anchor
		// over it, and the receipt read as verified. This is the check the whole receipt rests on.
		if err != nil || hex.EncodeToString(merkle.LeafHash(leafData)) != c.Chain.Link {
			return false, c.Chain.Seq, fmt.Sprintf("the entry at seq %d does not recompute to its "+
				"link", c.Chain.Seq)
		}

		path, err := decodeProofHashes(c.Inclusion.Path)
		// A claim's sequence is one based and the tree is zero based.
		if err != nil || !merkle.VerifyInclusion(leafData, c.Chain.Seq-1, size, path, root) {
			return false, c.Chain.Seq, fmt.Sprintf("the entry at seq %d does not fold to the root "+
				"the head names", c.Chain.Seq)
		}
	}
	// A carried consistency proof is verified, not merely displayed. Its from-root becomes an
	// admissible anchor coordinate below, and admitting a root the proof does not actually fold to
	// the head would let a producer pair a fabricated history with a genuine-looking anchor.
	if c := b.Chain.Consistency; c != nil {
		fromRoot, rerr := hex.DecodeString(c.FromRoot)
		path, perr := decodeProofHashes(c.Path)
		if rerr != nil || len(fromRoot) == 0 || perr != nil ||
			!merkle.VerifyConsistency(c.FromSize, size, fromRoot, root, path) {
			return false, 0, fmt.Sprintf("the consistency proof does not show the log growing from "+
				"%d entries to %d by appending only", c.FromSize, size)
		}
	}
	return true, 0, ""
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
// claim's link as its own previous. It returns the sequence of the claim the check stopped at and
// what was wrong there.
//
// The reason is returned beside the position because the position cannot speak for itself. It was
// read as the claim whose link did not recompute, and the claim after a gap recomputes perfectly: a
// bundle with seq 10 cut out was reported as broken at seq 11, which sent a reader to the one entry
// with nothing wrong with it.
func verifyBundleChain(claims []BundleClaim, head BundleCoord) (ok bool, brokeAt int64, problem string) {
	for i, c := range claims {
		// The genesis rule and contiguous ascending sequence numbers, the two checks the loomseal
		// reference verifier enforces that recomputing each link from the claim's own prev does not.
		// Without them a self-consistent chain with entries dropped between two it kept, or a window
		// opening past sequence one with an empty prev, recomputed cleanly and read as VERIFIED here
		// while the reference verifier the product tells relying parties to trust refused it.
		if i == 0 {
			if (c.Chain.Seq == 1) != (c.Chain.Prev == "") {
				if c.Chain.Seq == 1 {
					return false, c.Chain.Seq, "the entry at seq 1 names a previous link, which the " +
						"first entry of a chain cannot have"
				}
				return false, c.Chain.Seq, fmt.Sprintf("the first entry is at seq %d but names no "+
					"previous link, which only the entry at seq 1 may do", c.Chain.Seq)
			}
		} else if c.Chain.Seq != claims[i-1].Chain.Seq+1 {
			return false, c.Chain.Seq, sequenceProblem(claims, claims[i-1].Chain.Seq, c.Chain.Seq)
		}
		str := func(k string) string { s, _ := c.Payload[k].(string); return s }
		// The claim's values came from the document under test, not from this install, so a value
		// canonicalization refuses is a bad bundle rather than a bug here.
		recomputed, err := checkedLinkOf(claimObject(c.Chain.Seq, c.At,
			str("actor"), str("method"), str("path"), c.Chain.Prev,
			str("actor_type"), str("on_behalf_of"), str("content_digest"), str("install_id")))
		if err != nil || recomputed != c.Chain.Link {
			return false, c.Chain.Seq, fmt.Sprintf("the entry at seq %d does not recompute to its link",
				c.Chain.Seq)
		}
		if i > 0 && c.Chain.Prev != claims[i-1].Chain.Link {
			return false, c.Chain.Seq, fmt.Sprintf("the entry at seq %d names a previous link that is "+
				"not the link of seq %d", c.Chain.Seq, claims[i-1].Chain.Seq)
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
			return false, newest.Seq, fmt.Sprintf("the head names seq %d, behind the newest entry at "+
				"seq %d", head.Seq, newest.Seq)
		}
		if head.Seq == newest.Seq && head.Link != newest.Link {
			return false, newest.Seq, fmt.Sprintf("the head names seq %d with a link that is not the "+
				"link of the entry there", head.Seq)
		}
		return true, 0, ""
	}
	// A bundle carrying no claims proves nothing, so it must not report that nothing was altered.
	// The head is only constrained by the claims, so with none the loop above never ran and this
	// returned true for any subject and any head at all: a document naming a run that never happened,
	// at a sequence and link nobody produced, read as VERIFIED with only a parenthetical "(0 entries
	// recompute)" to give it away. A receipt is a claim about something; an empty one is not a true
	// claim about everything.
	if head.Seq != 0 || head.Link != "" {
		return false, head.Seq, fmt.Sprintf("no entries are carried, so the head at seq %d rests on "+
			"nothing", head.Seq)
	}
	return false, 0, "no entries are carried, so there is nothing to recompute"
}

// sequenceProblem says why the claim at seq does not follow the claim at prev.
//
// A gap names the sequence numbers it leaves out, since the claim after it recomputes and what is
// wrong is what is absent. A number that is absent at this point but carried later in the document
// is out of place rather than missing, and calling it missing would send a reader looking for an
// entry that is sitting in the file.
func sequenceProblem(claims []BundleClaim, prev, seq int64) string {
	switch {
	case seq == prev:
		return fmt.Sprintf("the entry at seq %d appears twice", seq)
	case seq < prev:
		return fmt.Sprintf("the entry at seq %d comes after seq %d, so the entries are out of order",
			seq, prev)
	}
	var displaced int64
	found := false
	for _, c := range claims {
		if c.Chain.Seq > prev && c.Chain.Seq < seq && (!found || c.Chain.Seq < displaced) {
			displaced, found = c.Chain.Seq, true
		}
	}
	switch {
	case found:
		return fmt.Sprintf("the entry at seq %d comes before seq %d, so the entries are out of order",
			seq, displaced)
	case prev+1 == seq-1:
		return fmt.Sprintf("the entry at seq %d is missing, so seq %d does not follow seq %d",
			prev+1, seq, prev)
	default:
		return fmt.Sprintf("the entries at seq %d through %d are missing, so seq %d does not follow "+
			"seq %d", prev+1, seq-1, seq, prev)
	}
}

// linearInstallMatches reports whether every claim that names an install names the signer's, and
// when one does not, the sequence of the first that does not.
//
// A claim that names one is bound, and the binding is load-bearing rather than cosmetic: the
// install id is one of the fields the chain link is computed over, in entryClaim, so rewriting it
// changes the link and the chain stops recomputing. That closes the lift where a second install
// republishes somebody else's history under its own key: keep the claims and the genuine anchor,
// rewrite the producer, re-sign, and a relying party pinning the second key would otherwise read
// the first install's record as the second's. Rewriting only the producer block fails this
// equality; rewriting the claims too fails the chain.
//
// A claim that names no install is grandfathered and continues, which is what lets a chain written
// across the upgrade verify rather than forcing a re-anchor. That is also the residual risk, stated
// plainly: an unbound entry is liftable, nothing here can retroactively bind it, because a link
// already written commits to what it committed to. The remedy is on the producing side, not this
// one. Every process that appends must bind its install, which is why serve and the demo both do it
// before their first write and warn loudly when they cannot.
func linearInstallMatches(b *Bundle, names installNames) (bool, int64) {
	for i := range b.Claims {
		named, _ := b.Claims[i].Payload["install_id"].(string)
		if named == "" {
			// An entry written before the binding existed. It hashes as it always did and is not
			// bound, which is a fact about that entry rather than a fault in this document.
			continue
		}
		// Either name this key has written under. An entry written before the id derivation was
		// widened names the install by its old width, and no later change can rewrite it.
		if !names.matches(named) {
			return false, b.Claims[i].Chain.Seq
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
	// Claim times by seq, for the backdate rule. See AnchorClockSkew.
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
			genTime.Before(at.Add(-AnchorClockSkew)) {
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
