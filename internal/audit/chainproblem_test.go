package audit

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// linkedClaims returns claims for the given sequence numbers, each naming the link of the claim
// before it as its previous, so every link recomputes and only the sequence structure varies. The
// first claim names a previous link unless it sits at seq 1, which is how a window opens.
func linkedClaims(seqs ...int64) []BundleClaim {
	claims := make([]BundleClaim, 0, len(seqs))
	prev := ""
	for i, seq := range seqs {
		if i == 0 && seq != 1 {
			prev = strings.Repeat("ab", 32)
		}
		c := validClaim(seq, prev)
		claims = append(claims, c)
		prev = c.Chain.Link
	}
	return claims
}

// TestVerifyBundleChainSaysWhatBrokeAndWhere pins the reason the linear chain check gives beside the
// position it stopped at.
//
// The position alone was the whole report, and the verify command printed it as "does not recompute
// at seq N" whatever the fault was. A bundle with seq 10 cut out read as though seq 11 had been
// altered, when seq 11 recomputes and the fault is the entry that is gone, and a swapped pair read
// the same way. A reader sent to the wrong entry concludes the tool is wrong about it.
//
//nolint:funlen // Test function.
func TestVerifyBundleChainSaysWhatBrokeAndWhere(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Claims      []BundleClaim
		Head        *BundleCoord
		WantOK      bool
		WantSeq     int64
		WantProblem string
	}{{ // Test 0: A whole chain has nothing to report.
		Name: "whole", Claims: linkedClaims(1, 2, 3), WantOK: true,
	}, { // Test 1: A window whose head leads its claims is ordinary and reports nothing.
		Name: "window", Claims: linkedClaims(3, 4, 5), Head: &BundleCoord{Seq: 9, Link: "cd"},
		WantOK: true,
	}, { // Test 2: One entry cut out names that entry, not the one after it, which recomputes.
		Name: "one missing", Claims: linkedClaims(1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12), WantSeq: 11,
		WantProblem: "the entry at seq 10 is missing, so seq 11 does not follow seq 9",
	}, { // Test 3: A run of entries cut out names the run.
		Name: "several missing", Claims: linkedClaims(1, 2, 3, 4, 5, 6, 7, 8, 9, 13), WantSeq: 13,
		WantProblem: "the entries at seq 10 through 12 are missing, so seq 13 does not follow seq 9",
	}, { // Test 4: An entry carried later in the document is out of place rather than missing.
		Name: "swapped", Claims: linkedClaims(1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 10, 12), WantSeq: 11,
		WantProblem: "the entry at seq 11 comes before seq 10, so the entries are out of order",
	}, { // Test 5: A sequence number that goes backward.
		Name: "backward", Claims: linkedClaims(1, 2, 3, 2), WantSeq: 2,
		WantProblem: "the entry at seq 2 comes after seq 3, so the entries are out of order",
	}, { // Test 6: A sequence number carried twice.
		Name: "repeated", Claims: linkedClaims(1, 2, 2), WantSeq: 2,
		WantProblem: "the entry at seq 2 appears twice",
	}, { // Test 7: The genesis entry cannot name a predecessor.
		Name: "genesis with a previous link", Claims: []BundleClaim{validClaim(1, "deadbeef")},
		WantSeq:     1,
		WantProblem: "the entry at seq 1 names a previous link, which the first entry of a chain cannot have",
	}, { // Test 8: A window cannot pass itself off as the start of the chain.
		Name: "window with no previous link", Claims: []BundleClaim{validClaim(5, "")}, WantSeq: 5,
		WantProblem: "the first entry is at seq 5 but names no previous link, which only the entry " +
			"at seq 1 may do",
	}, { // Test 9: An entry edited after the fact is the one fault that is a link not recomputing.
		Name: "edited", Claims: func() []BundleClaim {
			claims := linkedClaims(1, 2, 3)
			claims[1].Payload["path"] = "/v1/somewhere-else"
			return claims
		}(), WantSeq: 2,
		WantProblem: "the entry at seq 2 does not recompute to its link",
	}, { // Test 10: An entry whose own link recomputes but that names another predecessor.
		Name: "relinked", Claims: func() []BundleClaim {
			claims := linkedClaims(1)
			return append(claims, validClaim(2, strings.Repeat("ff", 32)))
		}(), WantSeq: 2,
		WantProblem: "the entry at seq 2 names a previous link that is not the link of seq 1",
	}, { // Test 11: A head behind the newest entry, which a chain that only grows cannot have.
		Name: "head behind", Claims: linkedClaims(1, 2, 3), Head: &BundleCoord{Seq: 2, Link: "cd"},
		WantSeq:     3,
		WantProblem: "the head names seq 2, behind the newest entry at seq 3",
	}, { // Test 12: A head level with the newest entry has to be that entry.
		Name: "head link", Claims: linkedClaims(1, 2, 3), Head: &BundleCoord{Seq: 3, Link: "cd"},
		WantSeq:     3,
		WantProblem: "the head names seq 3 with a link that is not the link of the entry there",
	}, { // Test 13: No entries under a head proves nothing about it.
		Name: "empty with a head", Head: &BundleCoord{Seq: 9999, Link: "cafebabe"}, WantSeq: 9999,
		WantProblem: "no entries are carried, so the head at seq 9999 rests on nothing",
	}, { // Test 14: No entries and no head.
		Name: "empty", Head: &BundleCoord{},
		WantProblem: "no entries are carried, so there is nothing to recompute",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			head := BundleCoord{}
			switch {
			case test.Head != nil:
				head = *test.Head
			case len(test.Claims) > 0:
				head = headOf(test.Claims)
			}
			ok, seq, problem := verifyBundleChain(test.Claims, head)
			if ok != test.WantOK || seq != test.WantSeq || problem != test.WantProblem {
				t.Errorf("verifyBundleChain() = %v, %d, %q\nwant %v, %d, %q", ok, seq, problem,
					test.WantOK, test.WantSeq, test.WantProblem)
			}
		})
	}
}

// TestVerifyBundleNamesTheEntryUnderAForeignInstall pins the reason for the one linear refusal made
// outside the link walk. It reported the head's sequence, so a lifted entry at seq 2 read as a chain
// that did not recompute at the far end.
func TestVerifyBundleNamesTheEntryUnderAForeignInstall(t *testing.T) {
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	entries := buildChain(t, 4)
	for _, e := range entries {
		e.InstallID = id.InstallID
	}
	entries[1].InstallID = "in_ffffffffffff"
	var prev *Entry
	for _, e := range entries {
		Link(prev, e)
		prev = e
	}
	doc, err := BuildBundle(entries, id, "1.101.0", time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	want := "the entry at seq 2 names an install the signing key does not speak for"
	if rep.ChainOK || rep.BrokeAtSeq != 2 || rep.ChainProblem != want {
		t.Errorf("chain ok %v at seq %d, %q\nwant false at seq 2, %q", rep.ChainOK, rep.BrokeAtSeq,
			rep.ChainProblem, want)
	}
}

// TestVerifyBundleTreeSaysWhatBrokeAndWhere is the tree profile's half. A sparse receipt's refusals
// came back as a position alone, and several of them as position zero, so the verify command told a
// reader a chain did not recompute at seq 0, a sequence no chain has.
//
//nolint:funlen // Test function.
func TestVerifyBundleTreeSaysWhatBrokeAndWhere(t *testing.T) {
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	chain := buildChain(t, 10)
	tests := []struct {
		Name        string
		Consistency int64
		Tamper      func(b *Bundle)
		WantOK      bool
		WantSeq     int64
		WantProblem string
	}{{ // Test 0: The control. An untouched sparse receipt reports nothing.
		Name: "untouched", Consistency: 6, Tamper: func(*Bundle) {}, WantOK: true,
	}, { // Test 1: A claim whose declared link is not its own leaf.
		Name: "declared link", Tamper: func(b *Bundle) { b.Claims[1].Chain.Link = strings.Repeat("f", 64) },
		WantSeq: 4, WantProblem: "the entry at seq 4 does not recompute to its link",
	}, { // Test 2: A claim carrying the predecessor a tree has no place for.
		Name: "previous link", Tamper: func(b *Bundle) { b.Claims[0].Chain.Prev = strings.Repeat("a", 64) },
		WantSeq: 2, WantProblem: "the entry at seq 2 carries a previous link, which a tree entry cannot have",
	}, { // Test 3: A claim with its audit path taken away.
		Name: "no audit path", Tamper: func(b *Bundle) { b.Claims[1].Inclusion = nil },
		WantSeq: 4, WantProblem: "the entry at seq 4 carries no audit path",
	}, { // Test 4: A head whose root the claims do not fold to.
		Name: "head root", Tamper: func(b *Bundle) { b.Chain.Head.Link = strings.Repeat("b", 64) },
		WantSeq: 2, WantProblem: "the entry at seq 2 does not fold to the root the head names",
	}, { // Test 5: A head whose root is not a hash at all.
		Name: "unreadable root", Tamper: func(b *Bundle) { b.Chain.Head.Link = "not a root" },
		WantProblem: "the head names a root that is not a hash",
	}, { // Test 6: Claims out of order.
		Name: "reordered", Tamper: func(b *Bundle) { b.Claims[0], b.Claims[1] = b.Claims[1], b.Claims[0] },
		WantSeq: 2, WantProblem: "the entry at seq 2 comes after seq 4, so the entries are out of order",
	}, { // Test 7: A claim presented twice.
		Name: "repeated", Tamper: func(b *Bundle) { b.Claims[1] = b.Claims[0] },
		WantSeq: 2, WantProblem: "the entry at seq 2 appears twice",
	}, { // Test 8: Nothing disclosed.
		Name: "empty", Tamper: func(b *Bundle) { b.Claims = []BundleClaim{} },
		WantProblem: "no entries are carried, so there is nothing to recompute",
	}, { // Test 9: Leaves with no install to bind to.
		Name: "no install", Tamper: func(b *Bundle) { delete(b.Chain.Params, "install_id") },
		WantProblem: "the chain does not bind its leaves to the producer's install",
	}, { // Test 10: A consistency proof that does not start where it says.
		Name: "consistency", Consistency: 6,
		Tamper:      func(b *Bundle) { b.Chain.Consistency.FromRoot = strings.Repeat("c", 64) },
		WantProblem: "the consistency proof does not show the log growing from 6 entries to 10 by appending only",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			doc, err := BuildTreeBundle(chain, map[int64]bool{2: true, 4: true, 7: true}, id, "1.101.0",
				BundleSubject{Type: "run", ID: "run_2"}, time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("BuildTreeBundle() error = %v", err)
			}
			if test.Consistency > 0 {
				if err := doc.AttachConsistency(chain, test.Consistency, id); err != nil {
					t.Fatalf("AttachConsistency() error = %v", err)
				}
			}
			test.Tamper(doc)
			signed, err := SignBundleDoc(doc, id.Private())
			if err != nil {
				t.Fatalf("SignBundleDoc() error = %v", err)
			}
			rep, err := VerifyBundle(signed, id.KeyID())
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if rep.ChainOK != test.WantOK || rep.BrokeAtSeq != test.WantSeq ||
				rep.ChainProblem != test.WantProblem {
				t.Errorf("chain ok %v at seq %d, %q\nwant %v at seq %d, %q", rep.ChainOK,
					rep.BrokeAtSeq, rep.ChainProblem, test.WantOK, test.WantSeq, test.WantProblem)
			}
		})
	}
}
