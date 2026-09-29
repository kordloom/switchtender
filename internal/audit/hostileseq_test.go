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
	_ = linkOf(claimObject(1<<60, "2026-08-24T00:00:00Z", "a", "POST", "/p", "", "", "", "", ""))
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

// validClaim builds a bundle claim whose link is the correct recompute of its own fields, so the
// only thing a test varies is the sequence-and-prev structure the new rules police.
func validClaim(seq int64, prev string) BundleClaim {
	const at, actor, method, path = "2026-08-24T00:00:00Z", "root", "POST", "/v1/runs"
	link := linkOf(claimObject(seq, at, actor, method, path, prev, "", "", "", ""))
	return BundleClaim{
		At:      at,
		Chain:   BundleCoordLink{Seq: seq, Link: link, Prev: prev},
		Payload: map[string]any{"actor": actor, "method": method, "path": path},
	}
}

// TestVerifyBundleChainEnforcesGenesisAndContinuity pins the two rules the loomseal reference
// verifier enforces that recomputing each link alone does not: the genesis rule and contiguous
// ascending sequence numbers.
//
// Every claim below has a correctly recomputed link and a head consistent with its newest claim, so
// the link check and the head check both pass and only the new rules can reject. Without them a
// self-consistent chain with entries dropped between two it kept, or a window opening past sequence
// one with an empty prev, read as VERIFIED here while the reference verifier the product tells
// relying parties to run refused it: a public counterexample to "verifiable by any open verifier".
func TestVerifyBundleChainEnforcesGenesisAndContinuity(t *testing.T) {
	t.Parallel()

	// Test 0: genesis carrying a prev link. Sequence one must have no prev.
	genesisWithPrev := []BundleClaim{validClaim(1, "deadbeefdeadbeef")}
	if ok, _ := verifyBundleChain(genesisWithPrev, headOf(genesisWithPrev)); ok {
		t.Error("a genesis claim carrying a prev link verified; the reference verifier refuses it")
	}

	// Test 1: a window opening past sequence one with an empty prev. It recomputes as though it
	// were genesis, presenting a truncated chain as unrooted.
	windowNoPrev := []BundleClaim{validClaim(5, "")}
	if ok, _ := verifyBundleChain(windowNoPrev, headOf(windowNoPrev)); ok {
		t.Error("a window opening at seq 5 with no prev verified; it hides everything before it")
	}

	// Test 2: non-contiguous sequences. claim0 is a valid genesis; claim1 links to it correctly but
	// its sequence jumps from 1 to 7, so entries 2 through 6 were dropped and re-linked around.
	c0 := validClaim(1, "")
	c1 := validClaim(7, c0.Chain.Link)
	gapped := []BundleClaim{c0, c1}
	if ok, _ := verifyBundleChain(gapped, headOf(gapped)); ok {
		t.Error("a chain with sequences 1 then 7 verified; entries were dropped between them")
	}

	// A genuinely contiguous, correctly rooted chain still verifies, so the rules reject only the
	// broken shapes and not every multi-claim bundle.
	g0 := validClaim(1, "")
	g1 := validClaim(2, g0.Chain.Link)
	good := []BundleClaim{g0, g1}
	if ok, brokeAt := verifyBundleChain(good, headOf(good)); !ok {
		t.Errorf("a contiguous rooted chain was refused at seq %d", brokeAt)
	}
}

// headOf returns a head consistent with a bundle's newest claim, so a test isolates the chain rules
// from the separate head-consistency check.
func headOf(claims []BundleClaim) BundleCoord {
	n := claims[len(claims)-1].Chain
	return BundleCoord{Seq: n.Seq, Link: n.Link}
}
