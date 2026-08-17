package audit_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// signedBundle builds and signs a bundle over the shared test chain, returning the bytes and the
// identity that signed it so a test can re-sign a tampered variant.
func signedBundle(t *testing.T) ([]byte, audit.Identity, *audit.Bundle) {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	b, err := audit.BuildBundle(bundleChain(t), id, "v-test", time.Date(2026, 7, 27, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(b, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	return signed, id, b
}

// TestVerifyBundleAcceptsAGenuineBundle proves a bundle this install signed verifies offline with no
// store and no network: the signature covers it, every chain link recomputes, and the pinned key
// matches. This is the yes a receipt's verify command answers.
func TestVerifyBundleAcceptsAGenuineBundle(t *testing.T) {
	signed, id, _ := signedBundle(t)

	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Errorf("report = %+v, want every check to pass", rep)
	}
	if !rep.SignatureOK || !rep.ChainOK || !rep.AnchorsOK {
		t.Errorf("signature=%v chain=%v anchors=%v, want all true",
			rep.SignatureOK, rep.ChainOK, rep.AnchorsOK)
	}
	if rep.ClaimCount != 4 {
		t.Errorf("claim count = %d, want 4", rep.ClaimCount)
	}
}

// TestVerifyBundleCatchesASignatureThatDoesNotCover flips a byte of the signature and checks the
// bundle no longer verifies, so a bundle altered after signing is caught.
func TestVerifyBundleCatchesASignatureThatDoesNotCover(t *testing.T) {
	signed, _, _ := signedBundle(t)

	var doc map[string]any
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	sig := doc["signatures"].([]any)[0].(map[string]any)
	s := sig["sig"].(string)
	// Flip the first base64 character to a different one.
	flip := "B"
	if strings.HasPrefix(s, "B") {
		flip = "C"
	}
	sig["sig"] = flip + s[1:]
	tampered, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	rep, err := audit.VerifyBundle(tampered, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.SignatureOK {
		t.Error("a bundle with a broken signature reported SignatureOK")
	}
	if rep.OK() {
		t.Error("a bundle with a broken signature reported OK")
	}
}

// TestVerifyBundleCatchesATamperedClaim re-signs a bundle whose claim payload was altered, so the
// signature is valid but a link no longer recomputes. This isolates the chain check from the
// signature check: a producer who edits a past entry and re-signs is still caught.
func TestVerifyBundleCatchesATamperedClaim(t *testing.T) {
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	b, err := audit.BuildBundle(bundleChain(t), id, "v-test", time.Date(2026, 7, 27, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	// Edit a claim's recorded path but leave its committed link, then re-sign so the signature is
	// genuine over the altered bundle.
	b.Claims[1].Payload["path"] = "/v1/evil"
	signed, err := audit.SignBundleDoc(b, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.SignatureOK {
		t.Error("the re-signed bundle should have a valid signature")
	}
	if rep.ChainOK {
		t.Error("a bundle whose claim was altered reported ChainOK")
	}
	if rep.BrokeAtSeq != b.Claims[1].Chain.Seq {
		t.Errorf("broke at seq %d, want %d", rep.BrokeAtSeq, b.Claims[1].Chain.Seq)
	}
}

// TestVerifyBundleRefusesAnUnpinnedKey checks a bundle signed by a different key than the one a
// relying party pinned is refused before its own signature is trusted.
func TestVerifyBundleRefusesAnUnpinnedKey(t *testing.T) {
	signed, _, _ := signedBundle(t)
	if _, err := audit.VerifyBundle(signed, "sha256:not-the-key"); !errors.Is(err, audit.ErrVerify) {
		t.Errorf("VerifyBundle() with a wrong pinned key error = %v, want ErrVerify", err)
	}
}

// TestVerifyBundleChecksOutcomeDisclosure proves a receipt that discloses a run's outcome is only
// trusted when the disclosed body matches the digest the chain committed. Tampering the body and
// re-signing keeps the signature and chain valid but must still fail, because the disclosure is a
// claim about what the run did that the chain has to back.
func TestVerifyBundleChecksOutcomeDisclosure(t *testing.T) {
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"run_id":"run_x","status":"succeeded","exit_code":0,"log_sha256":"abc",` +
		`"hosts":[{"host":"web01","worst":"ok","ok":1,"changed":0,"failures":0,"unreachable":0,"skipped":0}]}`)
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}

	creation := &audit.Entry{ID: "c", At: at, Actor: "alice", Method: "POST", Path: "/v1/runs"}
	audit.Link(nil, creation)
	outcomeE := &audit.Entry{
		ID: "o", At: at.Add(time.Minute), Actor: "system:dispatcher", ActorType: "system",
		OnBehalfOf: "alice", Method: audit.MethodRun, Path: "/runs/run_x/outcome/succeeded",
		ContentDigest: digest, Nonce: nonce,
	}
	audit.Link(creation, outcomeE)

	build := func(mutate func(m map[string]any)) []byte {
		doc, berr := audit.BuildBundle([]*audit.Entry{creation, outcomeE}, id, "v", at)
		if berr != nil {
			t.Fatalf("BuildBundle() error = %v", berr)
		}
		var bodyObj any
		if uerr := json.Unmarshal(body, &bodyObj); uerr != nil {
			t.Fatalf("Unmarshal() error = %v", uerr)
		}
		for i := range doc.Claims {
			if doc.Claims[i].Chain.Seq == outcomeE.Seq {
				doc.Claims[i].Payload["outcome_body"] = bodyObj
				doc.Claims[i].Payload["outcome_nonce"] = nonce
				if mutate != nil {
					mutate(bodyObj.(map[string]any))
				}
			}
		}
		signed, serr := audit.SignBundleDoc(doc, id.Private())
		if serr != nil {
			t.Fatalf("SignBundleDoc() error = %v", serr)
		}
		return signed
	}

	// Genuine: the disclosure is present and matches, and the whole receipt is OK.
	rep, err := audit.VerifyBundle(build(nil), "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OutcomePresent || !rep.OutcomeDigestOK || !rep.OK() {
		t.Errorf("genuine disclosure: present=%v digestOK=%v ok=%v, want all true",
			rep.OutcomePresent, rep.OutcomeDigestOK, rep.OK())
	}

	// Tampered body, re-signed: signature and chain still hold, but the disclosure does not, so the
	// receipt is not OK.
	rep, err = audit.VerifyBundle(build(func(m map[string]any) { m["status"] = "failed" }), "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.SignatureOK || !rep.ChainOK {
		t.Errorf("re-signed bundle: signature=%v chain=%v, want both true", rep.SignatureOK, rep.ChainOK)
	}
	if rep.OutcomeDigestOK {
		t.Error("a tampered disclosed outcome reported OutcomeDigestOK")
	}
	if rep.OK() {
		t.Error("a receipt with a mismatched outcome disclosure reported OK")
	}
}

// TestVerifyBundleCatchesASpecInconsistency proves the receipt's three statements about the spec
// must agree: a bundle whose decision binds one digest while its outcome commits another is
// internally coherent claim by claim, and still refused, because the approval and the execution it
// ties together are not about the same change.
func TestVerifyBundleCatchesASpecInconsistency(t *testing.T) {
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	decisionBody := []byte(`{"run_id":"run_x","verdict":"approved","spec_digest":"sha256:aaaa"}`)
	decisionDigest, decisionNonce, err := audit.ContentDigestOf(decisionBody)
	if err != nil {
		t.Fatalf("ContentDigestOf(decision) error = %v", err)
	}
	outcomeBody := []byte(`{"run_id":"run_x","status":"succeeded","exit_code":0,` +
		`"log_sha256":"abc","spec_digest":"sha256:bbbb"}`)
	outcomeDigest, outcomeNonce, err := audit.ContentDigestOf(outcomeBody)
	if err != nil {
		t.Fatalf("ContentDigestOf(outcome) error = %v", err)
	}

	decision := &audit.Entry{
		ID: "d", At: at, Actor: "approver-pat", ActorType: "session",
		Method: audit.MethodDecision, Path: "/runs/run_x/decision/approved",
		ContentDigest: decisionDigest, Nonce: decisionNonce,
	}
	audit.Link(nil, decision)
	outcomeE := &audit.Entry{
		ID: "o", At: at.Add(time.Minute), Actor: "system:dispatcher", ActorType: "system",
		Method: audit.MethodRun, Path: "/runs/run_x/outcome/succeeded",
		ContentDigest: outcomeDigest, Nonce: outcomeNonce,
	}
	audit.Link(decision, outcomeE)

	doc, err := audit.BuildBundle([]*audit.Entry{decision, outcomeE}, id, "v", at)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	attach := func(seq int64, key string, body []byte, nonceKey, nonce string) {
		var obj any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		for i := range doc.Claims {
			if doc.Claims[i].Chain.Seq == seq {
				doc.Claims[i].Payload[key] = obj
				doc.Claims[i].Payload[nonceKey] = nonce
			}
		}
	}
	attach(decision.Seq, "decision_body", decisionBody, "decision_nonce", decisionNonce)
	attach(outcomeE.Seq, "outcome_body", outcomeBody, "outcome_nonce", outcomeNonce)
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.DecisionsOK || rep.DecisionsPresent != 1 {
		t.Errorf("decisions = ok %v present %d, want the disclosure itself to verify", rep.DecisionsOK, rep.DecisionsPresent)
	}
	if !rep.OutcomeDigestOK {
		t.Error("the outcome disclosure itself should verify")
	}
	if rep.SpecConsistent {
		t.Error("a decision bound to one spec digest while the outcome commits another reported consistent")
	}
	if rep.OK() {
		t.Error("a receipt whose approved and executed specs disagree reported OK")
	}
}
