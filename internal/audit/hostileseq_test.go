package audit

import "testing"

// TestVerifyBundleRefusesAnUnrepresentableSeq checks a hostile receipt is reported as not verified
// rather than crashing the verifier.
//
// The chain link is computed over a canonical JSON object, and JCS refuses an integer above 2^53.
// On the producing side that cannot happen, because the sequence comes from this install's own
// store, so the helper panicked on the error as a programming fault. The verifier shares that helper
// and its sequence comes from a document somebody else wrote, so a receipt carrying a large sequence
// crashed `switchtender verify` with a stack trace. A relying party reads that as the tool being
// broken rather than the artifact being bad, which is a denial of the audit and the worst possible
// first impression of the one command the product asks people to trust.
func TestVerifyBundleRefusesAnUnrepresentableSeq(t *testing.T) {
	t.Parallel()
	claims := []BundleClaim{{
		At:      "2026-08-24T00:00:00Z",
		Chain:   BundleCoordLink{Seq: 1 << 60, Link: "aa", Prev: ""},
		Payload: map[string]any{"actor": "x", "method": "POST", "path": "/v1/runs"},
	}}

	// The point is that this returns rather than panicking.
	ok, brokeAt := verifyBundleChain(claims, BundleCoord{Seq: 1 << 60, Link: "aa"})
	if ok {
		t.Error("a claim whose sequence cannot be canonicalized was accepted")
	}
	if brokeAt != 1<<60 {
		t.Errorf("broke at seq = %d, want the offending claim's sequence", brokeAt)
	}
}

// TestLinkOfStillPanicsForOurOwnEntries keeps the producing side strict. A value this package cannot
// canonicalize is a fault here, and hashing the error text would mint a link nothing reproduces.
func TestLinkOfStillPanicsForOurOwnEntries(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("linkOf returned for a claim it cannot canonicalize, so a producer would store " +
				"a link nothing can recompute")
		}
	}()
	_ = linkOf(claimObject(1<<60, "2026-08-24T00:00:00Z", "a", "POST", "/p", "", "", "", ""))
}

// TestVerifyBundleChainRefusesAnEmptyBundle checks a document carrying no claims does not report
// that nothing was altered.
//
// The head is only constrained by the claims, so with none the recompute loop never ran and the
// chain check returned true for any subject and any head at all. A bundle naming a run that never
// happened, at a sequence and link nobody ever produced, read as VERIFIED, with only a parenthetical
// "(0 entries recompute)" to give it away. A receipt is a claim about something; an empty one is not
// a true claim about everything.
func TestVerifyBundleChainRefusesAnEmptyBundle(t *testing.T) {
	t.Parallel()
	if ok, _ := verifyBundleChain(nil, BundleCoord{Seq: 9999, Link: "cafebabe"}); ok {
		t.Error("a bundle with no claims verified against a head nobody produced")
	}
	if ok, _ := verifyBundleChain([]BundleClaim{}, BundleCoord{}); ok {
		t.Error("a bundle with no claims and no head verified")
	}
}
