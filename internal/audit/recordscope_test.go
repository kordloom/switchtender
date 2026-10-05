package audit

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestRecordsAreReadWhereTheChainSaysTheyAre pins which claims this product's verifier reads a
// record on, and what it refuses there. A claim is a record by the method and path its link
// commits, as the open format says, so a record member anywhere else is an ordinary member,
// reported unchecked and never failed. On a record claim, a member whose name differs from one the
// record is read for only in case fails, since a reader folding case would take one for the other.
// The unkeyed digest form is legacy: it verifies on an entry before the first keyed one, and is
// listed as legacy because its digest confirms a guess, and it fails on any entry after.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestRecordsAreReadWhereTheChainSaysTheyAre(t *testing.T) {
	tests := []struct {
		// UnkeyedDecision commits the decision under the legacy unkeyed form.
		UnkeyedDecision bool
		// KeyedRequest commits the request that created the run under the keyed form.
		KeyedRequest bool
		// Edit changes the claims after the bundle is built and before it is signed.
		Edit func(claims []BundleClaim)
		// WantOK is whether the document verifies.
		WantOK bool
		// WantLegacy are the records reported verified under the legacy form.
		WantLegacy []string
		// WantLegacyAfterKeyed are the records refused for the unkeyed form after a keyed entry.
		WantLegacyAfterKeyed []string
		// WantCaseVariants are the members refused for differing from a read member only in case.
		WantCaseVariants []string
		// WantUnchecked are the disclosed members reported unchecked, with why.
		WantUnchecked []string
		// WantUncheckedRecords is how many unchecked records the verdict counts.
		WantUncheckedRecords int
	}{{ // Test 0: An honest receipt verifies with nothing legacy and nothing unchecked.
		WantOK: true,
	}, { // Test 1: An unkeyed decision with nothing keyed before it verifies, listed as legacy.
		UnkeyedDecision: true, WantOK: true,
		WantLegacy: []string{"claim 1 decision_body"},
	}, { // Test 2: An unkeyed decision after a keyed entry is not from before nonces and fails.
		UnkeyedDecision: true, KeyedRequest: true,
		WantLegacyAfterKeyed: []string{"claim 1 decision_body"},
	}, { // Test 3: A spec on a claim that is no outcome is an ordinary member, never compared.
		Edit:   func(claims []BundleClaim) { claims[0].Payload["spec_body"] = `{"command":"other"}` },
		WantOK: true, WantUncheckedRecords: 1,
		WantUnchecked: []string{"claim 0 spec_body: " + elsewhereDetail},
	}, { // Test 4: A REASON entry that is no correction is not read, though it commits a digest.
		Edit: func(claims []BundleClaim) {
			claims[2].Payload["correction_body"] = map[string]any{"correction_id": "x"}
		},
		WantOK: true, WantUncheckedRecords: 1,
		WantUnchecked: []string{"claim 2 correction_body: " + elsewhereDetail},
	}, { // Test 5: A case variant beside the decision body fails the document.
		Edit: func(claims []BundleClaim) {
			claims[1].Payload["Decision_body"] = map[string]any{"verdict": "rejected"}
		},
		WantCaseVariants: []string{"claim 1 Decision_body"},
	}, { // Test 6: A nonce alone on a claim that is no record counts as an unchecked record.
		Edit:   func(claims []BundleClaim) { claims[0].Payload["decision_nonce"] = "ab" },
		WantOK: true, WantUncheckedRecords: 1,
		WantUnchecked: []string{"claim 0 decision_nonce: " + elsewhereDetail},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			rep := scopeReport(t, test.UnkeyedDecision, test.KeyedRequest, test.Edit)
			if rep.OK() != test.WantOK {
				t.Fatalf("OK() = %v, want %v: %+v", rep.OK(), test.WantOK, rep)
			}
			var unchecked []string
			for _, m := range rep.Disclosed {
				if m.State == MemberUnchecked {
					unchecked = append(unchecked, fmt.Sprintf("claim %d %s: %s", m.Claim, m.Member,
						m.Detail))
				}
			}
			for _, c := range []struct {
				name      string
				want, got []string
			}{
				{"legacy records", test.WantLegacy, rep.LegacyRecords},
				{"unkeyed after keyed", test.WantLegacyAfterKeyed, rep.LegacyAfterKeyed},
				{"case variants", test.WantCaseVariants, rep.CaseVariants},
			} {
				if diff := cmp.Diff(c.want, c.got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("%s mismatch (-want +got):\n%s", c.name, diff)
				}
			}
			if !test.WantOK {
				return
			}
			if diff := cmp.Diff(test.WantUnchecked, unchecked, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("unchecked members mismatch (-want +got):\n%s", diff)
			}
			if rep.DisclosedUnchecked != test.WantUncheckedRecords {
				t.Errorf("DisclosedUnchecked = %d, want %d", rep.DisclosedUnchecked,
					test.WantUncheckedRecords)
			}
			if i := slices.IndexFunc(rep.Disclosed, func(m DisclosedMember) bool {
				return m.Member == "decision_body"
			}); len(test.WantLegacy) > 0 && (i < 0 || rep.Disclosed[i].Detail != legacyDetail) {
				t.Errorf("a legacy decision body is not reported against the legacy form: %+v",
					rep.Disclosed)
			}
		})
	}
}

// scopeReport builds, signs, and verifies a run's receipt: the request that created the run, an
// approval decision with its body disclosed, a REASON entry recording a redaction, and the outcome
// committed under the exact form with the spec beside it. edit runs on the claims before signing.
func scopeReport(t *testing.T, unkeyedDecision, keyedRequest bool,
	edit func(claims []BundleClaim)) *BundleReport {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	const spec = `{"command":"deploy","tool":"bash"}`
	specDigest := UnkeyedDigestOfReduced([]byte(spec))
	raw, err := json.Marshal(map[string]string{"run_id": "run_s", "verdict": "approved",
		"spec_digest": specDigest})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	decisionDigest, decisionNonce := UnkeyedDigestOf(raw), ""
	if !unkeyedDecision {
		if decisionDigest, decisionNonce, err = ContentDigestOf(raw); err != nil {
			t.Fatalf("ContentDigestOf() error = %v", err)
		}
	}
	outcomeText := `{"run_id":"run_s","spec_digest":"` + specDigest + `","status":"succeeded"}`
	outcomeDigest, outcomeNonce, err := ExactDigestOf([]byte(outcomeText))
	if err != nil {
		t.Fatalf("ExactDigestOf() error = %v", err)
	}
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	request := &Entry{Actor: "deploy-bot", Method: "POST", Path: "/v1/runs"}
	if keyedRequest {
		if request.ContentDigest, request.Nonce, err = ContentDigestOf([]byte(`{"a":1}`)); err != nil {
			t.Fatalf("ContentDigestOf() error = %v", err)
		}
	}
	// The redaction entry commits a digest this product never writes on one, so a verifier that read
	// a correction on any REASON entry would hold a disclosed body against it and refuse.
	redaction := &Entry{Actor: "ops-admin", Method: MethodReason,
		Path: "/runs/run_s/decisions/aud_d/reason_redacted/personal_data"}
	if redaction.ContentDigest, redaction.Nonce, err = ContentDigestOf([]byte(`{"b":2}`)); err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	entries := []*Entry{request,
		{Actor: "ops-admin", Method: MethodDecision, Path: "/runs/run_s/decision/approved",
			ContentDigest: decisionDigest, Nonce: decisionNonce},
		redaction,
		{Actor: "system:dispatcher", Method: MethodRun, Path: "/runs/run_s/outcome/succeeded",
			ContentDigest: outcomeDigest, Nonce: outcomeNonce},
	}
	var prev *Entry
	for i, e := range entries {
		e.ID, e.Seq, e.At, e.InstallID = fmt.Sprintf("aud_%03d", i+1), int64(i+1),
			at.Add(time.Duration(i)*time.Second), id.InstallID
		Link(prev, e)
		prev = e
	}
	doc, err := BuildBundle(entries, id, "1.102.0", at.Add(time.Minute))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	doc.Claims[1].Payload["decision_body"] = body
	doc.Claims[1].Payload["decision_nonce"] = decisionNonce
	doc.Claims[3].Payload["outcome_body"] = outcomeText
	doc.Claims[3].Payload["outcome_nonce"] = outcomeNonce
	doc.Claims[3].Payload["spec_body"] = spec
	if edit != nil {
		edit(doc.Claims)
	}
	signed, err := SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	return rep
}
