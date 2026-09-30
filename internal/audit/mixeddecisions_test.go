package audit_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
)

// decisionSpec is a stand-in spec digest, distinct per run so a decision read back under the wrong
// run would show.
func decisionSpec(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// mixedDecisionBundle builds a whole-install bundle whose chain holds decisions written before
// decisions carried the account and decisions written after, each with its body disclosed the way a
// receipt discloses it. mutate runs on the built document before it is signed, nil for none.
func mixedDecisionBundle(t *testing.T, id audit.Identity, mutate func(*audit.Bundle)) []byte {
	t.Helper()
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	type decided struct {
		runID, route, verdict, actor, actorType, onBehalfOf string
	}
	decisions := []decided{
		// Before: a person's session, and an install serving open, which recorded no one.
		{"run_old", "approve", "approved", "pat", "session", ""},
		{"run_open_old", "reject", "rejected", "", "", ""},
		// After: a token bound to an account, and an install serving open, which now records the
		// name its request carries.
		{"run_new", "approve", "approved", "laptop", "token", "alice"},
		{"run_open_new", "approve", "approved", "unauthenticated", "unauthenticated", ""},
	}
	bodies := map[string]any{}
	nonces := map[string]string{}
	var entries []*audit.Entry
	var prev *audit.Entry
	add := func(e *audit.Entry) {
		e.ID = audit.NewID()
		e.At = at.Add(time.Duration(len(entries)) * time.Minute)
		e.InstallID = id.InstallID
		audit.Link(prev, e)
		entries = append(entries, e)
		prev = e
	}
	for _, d := range decisions {
		raw, err := json.Marshal(map[string]string{
			"run_id": d.runID, "verdict": d.verdict, "spec_digest": decisionSpec(d.runID),
		})
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		digest, nonce, err := audit.ContentDigestOf(raw)
		if err != nil {
			t.Fatalf("ContentDigestOf() error = %v", err)
		}
		var body any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		path := "/runs/" + d.runID + "/decision/" + d.verdict
		bodies[path], nonces[path] = body, nonce
		add(&audit.Entry{Actor: d.actor, ActorType: d.actorType, OnBehalfOf: d.onBehalfOf,
			Method: "POST", Path: "/v1/runs/" + d.runID + "/" + d.route})
		add(&audit.Entry{Actor: d.actor, ActorType: d.actorType, OnBehalfOf: d.onBehalfOf,
			Method: audit.MethodDecision, Path: path, ContentDigest: digest, Nonce: nonce})
	}
	doc, err := audit.BuildBundle(entries, id, "1.101.0", at.Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	for i := range doc.Claims {
		path, _ := doc.Claims[i].Payload["path"].(string)
		if body, ok := bodies[path]; ok {
			doc.Claims[i].Payload["decision_body"] = body
			doc.Claims[i].Payload["decision_nonce"] = nonces[path]
		}
	}
	if mutate != nil {
		mutate(doc)
	}
	return signBundle(t, doc, id)
}

// TestDecisionsBeforeAndAfterTheAccountVerifyTogether pins that the account a decision now carries
// breaks nothing written before it did.
//
// on_behalf_of was already one of the fields a link commits to, so a decision that carries it and
// one that predates it are both ordinary claims under the same profile. Both verifiers have to
// accept a chain holding the two side by side, which is what every upgraded install will hold, and
// this product has to read the account back where there is one and nothing where there is none.
func TestDecisionsBeforeAndAfterTheAccountVerifyTogether(t *testing.T) {
	id := treeIdentity(t)
	signed := mixedDecisionBundle(t, id, nil)

	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Fatalf("our verifier refused a chain mixing old and new decisions: chain %v (%s), "+
			"decisions %v", rep.ChainOK, rep.ChainProblem, rep.DecisionsOK)
	}
	want := []audit.DisclosedDecision{
		{Actor: "pat", ActorType: "session", Verdict: "approved", SpecDigest: decisionSpec("run_old")},
		{Verdict: "rejected", SpecDigest: decisionSpec("run_open_old")},
		{Actor: "laptop", ActorType: "token", OnBehalfOf: "alice", Verdict: "approved",
			SpecDigest: decisionSpec("run_new")},
		{Actor: "unauthenticated", ActorType: "unauthenticated", Verdict: "approved",
			SpecDigest: decisionSpec("run_open_new")},
	}
	if diff := cmp.Diff(want, rep.Decisions); diff != "" {
		t.Errorf("decisions read back mismatch (-want +got):\n%s", diff)
	}
	if theirs := verifyWithLoomSeal(t, signed); !theirs.OK {
		t.Errorf("the reference verifier refused a chain mixing old and new decisions: %v",
			theirs.Problems)
	}

	// The control: the account is committed, so rewriting it after the fact is refused by both,
	// and this test is not passing over a field neither verifier reads.
	forged := mixedDecisionBundle(t, id, func(b *audit.Bundle) {
		for i := range b.Claims {
			if b.Claims[i].Payload["on_behalf_of"] == "alice" {
				b.Claims[i].Payload["on_behalf_of"] = "mallory"
			}
		}
	})
	rep, err = audit.VerifyBundle(forged, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle(forged) error = %v", err)
	}
	if rep.ChainOK {
		t.Error("our verifier accepted a decision whose account was rewritten after signing")
	}
	if theirs := verifyWithLoomSeal(t, forged); theirs.OK {
		t.Error("the reference verifier accepted a decision whose account was rewritten after signing")
	}
}
