package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/witness"
)

// testAttestAt is when the test witness countersigns, after every entry treeChain writes.
var testAttestAt = time.Date(2026, 8, 11, 12, 0, 0, 500_000_000, time.UTC)

// testProducerIdentity returns a producer identity read from a directory of its own and never from
// the environment, so a parallel test may call it.
func testProducerIdentity(t *testing.T) audit.Identity {
	t.Helper()
	id, err := identity.LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("identity.LoadFile() error = %v", err)
	}
	return id
}

// testWitnessIdentity returns a witness identity of its own, a key no producer in the test holds.
func testWitnessIdentity(t *testing.T) audit.Identity {
	t.Helper()
	id, err := identity.LoadWitnessFile(t.TempDir())
	if err != nil {
		t.Fatalf("identity.LoadWitnessFile() error = %v", err)
	}
	return id
}

// testClaimAttestation has w countersign link the way a push witness does and records the answer at
// seq, the way an install stores it.
func testClaimAttestation(t *testing.T, w audit.Identity, seq int64, link string) *audit.WitnessAttestation {
	t.Helper()
	cs, err := witness.ClaimAttestation(link, testAttestAt, w)
	if err != nil {
		t.Fatalf("witness.ClaimAttestation() error = %v", err)
	}
	return &audit.WitnessAttestation{
		ID: audit.NewAttestationID(), Seq: seq, Link: link, Kind: audit.AttestationClaim,
		KeyID: cs.KeyID, PublicKey: cs.PublicKey, Role: cs.Role, At: cs.At, Sig: cs.Sig,
		Witness: cs.KeyID,
	}
}

// testHeadAttestation has w countersign the tree root over size entries and records the answer.
func testHeadAttestation(t *testing.T, w audit.Identity, size int64, root string) *audit.WitnessAttestation {
	t.Helper()
	cs, err := witness.HeadAttestation(root, size, testAttestAt, w)
	if err != nil {
		t.Fatalf("witness.HeadAttestation() error = %v", err)
	}
	return &audit.WitnessAttestation{
		ID: audit.NewAttestationID(), Seq: size, Kind: audit.AttestationHead, Size: size,
		Root: root, KeyID: cs.KeyID, PublicKey: cs.PublicKey, Role: cs.Role, At: cs.At,
		Sig: cs.Sig, Witness: cs.KeyID,
	}
}

// testTreeRoot returns the root of the tree over entries as producer's install sees it.
func testTreeRoot(t *testing.T, entries []*audit.Entry, producer audit.Identity) string {
	t.Helper()
	_, root, err := audit.TreeHead(entries, producer.InstallID)
	if err != nil {
		t.Fatalf("TreeHead() error = %v", err)
	}
	return root
}

// testAttestedBundles builds the bundle shapes attestations are offered to, each fresh, over chain.
type testAttestedBundles struct {
	// Chain is the log every shape is drawn from.
	Chain []*audit.Entry
	// Producer signs and binds every shape.
	Producer audit.Identity
}

// Linear returns the linear bundle over entries.
func (s testAttestedBundles) Linear(t *testing.T, entries []*audit.Entry) *audit.Bundle {
	t.Helper()
	doc, err := audit.BuildBundle(entries, s.Producer, "1.34.1", testAttestAt)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	return doc
}

// Tree returns the tree bundle over the whole chain disclosing seqs.
func (s testAttestedBundles) Tree(t *testing.T, seqs ...int64) *audit.Bundle {
	t.Helper()
	disclose := make(map[int64]bool, len(seqs))
	for _, seq := range seqs {
		disclose[seq] = true
	}
	doc, err := audit.BuildTreeBundle(s.Chain, disclose, s.Producer, "1.34.1",
		audit.BundleSubject{Type: "run", ID: "run_attested"}, testAttestAt)
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	return doc
}

// decodeCountersignature reads one attestation member a bundle carries back into the witness's
// own shape, so it can be checked under the rule a LoomSeal verifier applies.
func decodeCountersignature(t *testing.T, raw json.RawMessage) witness.Countersignature {
	t.Helper()
	var cs witness.Countersignature
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatalf("attestation member is not JSON: %v\n%s", err, raw)
	}
	return cs
}

// TestWitnessAttestationsRideOnTheirClaims pins that a witness attestation goes on exactly the
// claim it names and nowhere else, and that a head attestation goes on only a tree bundle that
// holds every claim through the size it signs. Every placed attestation is then checked under the
// LoomSeal rule against the link or head it sits on, so a placement that would fail at a verifier
// fails here.
func TestWitnessAttestationsRideOnTheirClaims(t *testing.T) {
	t.Parallel()
	chain := treeChain(t, 6)
	producer := testProducerIdentity(t)
	w, rotated := testWitnessIdentity(t), testWitnessIdentity(t)
	shapes := testAttestedBundles{Chain: chain, Producer: producer}
	root6, root5 := testTreeRoot(t, chain, producer), testTreeRoot(t, chain[:5], producer)
	link := func(seq int64) string { return chain[seq-1].Hash }

	wrongKind := testClaimAttestation(t, w, 2, link(2))
	wrongKind.Kind = audit.AnchorRFC3161
	offHead := testHeadAttestation(t, w, 6, root6)
	offHead.Seq = 5
	offSize := testHeadAttestation(t, w, 6, root6)
	offSize.Size = 5

	claim := func(by audit.Identity, seq int64, l string) *audit.WitnessAttestation {
		return testClaimAttestation(t, by, seq, l)
	}
	head := func(size int64, root string) *audit.WitnessAttestation {
		return testHeadAttestation(t, w, size, root)
	}
	offer := func(a ...*audit.WitnessAttestation) []*audit.WitnessAttestation { return a }
	linear := func(t *testing.T) *audit.Bundle { return shapes.Linear(t, chain) }
	complete := func(t *testing.T) *audit.Bundle { return shapes.Tree(t, 1, 2, 3, 4, 5, 6) }
	sparse := func(t *testing.T) *audit.Bundle { return shapes.Tree(t, 2, 4) }

	tests := []struct {
		// Bundle builds the bundle the attestations are offered to.
		Bundle func(t *testing.T) *audit.Bundle
		// Recorded are the attestations offered, as the store returns them.
		Recorded []*audit.WitnessAttestation
		// WantClaims maps each claim seq to the Recorded indexes it carries afterward, in order.
		WantClaims map[int64][]int
		// WantHead is the indexes into Recorded the bundle carries at its head afterward, in order.
		WantHead []int
		// WantPlaced is the count AttachAttestations reports.
		WantPlaced int
	}{{ // Test 0: Two claim attestations go on the two claims they name.
		Bundle: linear, Recorded: offer(claim(w, 2, link(2)), claim(w, 5, link(5))),
		WantClaims: map[int64][]int{2: {0}, 5: {1}}, WantPlaced: 2,
	}, { // Test 1: An attestation whose seq and link name different claims goes nowhere.
		Bundle: linear, Recorded: offer(claim(w, 3, link(4))),
	}, { // Test 2: An attestation over a link the chain no longer holds goes nowhere.
		Bundle: linear, Recorded: offer(claim(w, 2, strings.Repeat("e", 64))),
	}, { // Test 3: An attestation past the bundle's head goes nowhere.
		Bundle: linear, Recorded: offer(claim(w, 7, strings.Repeat("d", 64))),
	}, { // Test 4: A window carries the attestation inside it and drops the one before it.
		Bundle:     func(t *testing.T) *audit.Bundle { return shapes.Linear(t, chain[2:]) },
		Recorded:   offer(claim(w, 1, link(1)), claim(w, 4, link(4))),
		WantClaims: map[int64][]int{4: {1}}, WantPlaced: 1,
	}, { // Test 5: Two witness keys over one claim, as a rotation leaves them, both ride on it.
		Bundle: linear, Recorded: offer(claim(w, 3, link(3)), claim(rotated, 3, link(3))),
		WantClaims: map[int64][]int{3: {0, 1}}, WantPlaced: 2,
	}, { // Test 6: A record of a kind that is not an attestation goes nowhere.
		Bundle: linear, Recorded: offer(wrongKind),
	}, { // Test 7: A nil record is skipped and the one after it still placed.
		Bundle: linear, Recorded: offer(nil, claim(w, 1, link(1))),
		WantClaims: map[int64][]int{1: {1}}, WantPlaced: 1,
	}, { // Test 8: A sparse claim's link is its leaf hash, which no witness signs.
		Bundle: sparse, Recorded: offer(claim(w, 2, link(2)), claim(w, 4, link(4))),
	}, { // Test 9: A tree bundle complete through the witnessed size carries the head attestation.
		Bundle: complete, Recorded: offer(head(6, root6)), WantHead: []int{0}, WantPlaced: 1,
	}, { // Test 10: A head attestation at a size other than the head's goes nowhere.
		Bundle: complete, Recorded: offer(head(5, root5)),
	}, { // Test 11: A sparse bundle is not complete through its head, so no head attestation.
		Bundle: sparse, Recorded: offer(head(6, root6)),
	}, { // Test 12: A linear head's link is an entry hash, which no head attestation signs.
		Bundle: linear, Recorded: offer(head(6, root6)),
	}, { // Test 13: A head attestation over a root the tree does not fold to goes nowhere.
		Bundle: complete, Recorded: offer(head(6, strings.Repeat("c", 64))),
	}, { // Test 14: A head record whose seq is not its size goes nowhere.
		Bundle: complete, Recorded: offer(offHead),
	}, { // Test 15: Nothing recorded places nothing.
		Bundle: linear,
	}, { // Test 16: A head record whose size is not its seq goes nowhere.
		Bundle: complete, Recorded: offer(offSize),
	}, { // Test 17: As many claims as the size, one position missing, is not complete.
		Bundle: func(t *testing.T) *audit.Bundle {
			doc := complete(t)
			doc.Claims[2] = doc.Claims[3]
			return doc
		},
		Recorded: offer(head(6, root6)),
	}, { // Test 18: A linear head carrying the root's value still means an entry hash.
		Bundle: func(t *testing.T) *audit.Bundle {
			doc := linear(t)
			doc.Chain.Head.Link = root6
			return doc
		},
		Recorded: offer(head(6, root6)),
	}, { // Test 19: A tree bundle disclosing a prefix of the log is not complete through its head.
		Bundle:   func(t *testing.T) *audit.Bundle { return shapes.Tree(t, 1, 2, 3) },
		Recorded: offer(head(6, root6)),
	}, { // Test 20: A bundle with no chain has no head to carry anything.
		Bundle:   func(*testing.T) *audit.Bundle { return &audit.Bundle{} },
		Recorded: offer(head(6, root6), claim(w, 1, link(1))),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc := test.Bundle(t)
			if got := doc.AttachAttestations(test.Recorded); got != test.WantPlaced {
				t.Errorf("AttachAttestations() = %d, want %d", got, test.WantPlaced)
			}
			gotClaims := map[int64][]string{}
			for _, c := range doc.Claims {
				for _, raw := range c.Attestations {
					cs := decodeCountersignature(t, raw)
					if err := witness.VerifyClaimAttestation(c.Chain.Link, cs); err != nil {
						t.Errorf("the attestation on claim %d does not verify over its link: %v",
							c.Chain.Seq, err)
					}
					gotClaims[c.Chain.Seq] = append(gotClaims[c.Chain.Seq], cs.Sig)
				}
			}
			var gotHead []string
			for _, raw := range doc.Attestations {
				cs := decodeCountersignature(t, raw)
				if err := witness.VerifyHeadAttestation(doc.Chain.Head.Link, doc.Chain.Head.Seq,
					cs); err != nil {
					t.Errorf("the head attestation does not verify over the head: %v", err)
				}
				gotHead = append(gotHead, cs.Sig)
			}
			wantClaims := map[int64][]string{}
			for seq, idx := range test.WantClaims {
				for _, i := range idx {
					wantClaims[seq] = append(wantClaims[seq], test.Recorded[i].Sig)
				}
			}
			var wantHead []string
			for _, i := range test.WantHead {
				wantHead = append(wantHead, test.Recorded[i].Sig)
			}
			if diff := cmp.Diff(wantClaims, gotClaims, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("claim attestations mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantHead, gotHead, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("head attestations mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAttachAttestationsReplacesWhatTheBundleCarried pins that attaching again leaves the bundle
// carrying the new answer alone, so a rider nobody recorded never survives into the signed bytes.
func TestAttachAttestationsReplacesWhatTheBundleCarried(t *testing.T) {
	t.Parallel()
	chain := treeChain(t, 6)
	producer := testProducerIdentity(t)
	w := testWitnessIdentity(t)
	shapes := testAttestedBundles{Chain: chain, Producer: producer}
	doc := shapes.Tree(t, 1, 2, 3, 4, 5, 6)
	stray := json.RawMessage(`{"key_id":"sha256:00","public_key":"","alg":"ed25519","role":"x",` +
		`"sig":""}`)
	doc.Claims[0].Attestations = []json.RawMessage{stray}
	doc.Attestations = []json.RawMessage{stray}
	if got := doc.AttachAttestations([]*audit.WitnessAttestation{
		testHeadAttestation(t, w, 6, testTreeRoot(t, chain, producer)),
	}); got != 1 {
		t.Fatalf("AttachAttestations() = %d, want 1", got)
	}
	if len(doc.Claims[0].Attestations) != 0 {
		t.Errorf("a claim kept %d attestations nobody recorded", len(doc.Claims[0].Attestations))
	}
	if len(doc.Attestations) != 1 || strings.Contains(string(doc.Attestations[0]), `"role":"x"`) {
		t.Errorf("the head carries %s, want only the recorded head attestation", doc.Attestations)
	}
	if got := doc.AttachAttestations(nil); got != 0 || len(doc.Attestations) != 0 {
		t.Errorf("AttachAttestations(nil) = %d with %d head attestations left, want none", got,
			len(doc.Attestations))
	}
}

// testAttestationStore is an audit store whose attestation reads answer what the test sets and
// record the bound each read was asked for.
type testAttestationStore struct {
	audit.Store
	// Recorded is what Attestations answers.
	Recorded []*audit.WitnessAttestation
	// Err is the error every attestation method answers with.
	Err error
	// GotThrough is the seq the last read was bounded by.
	GotThrough int64
}

// SaveAttestation answers Err and keeps nothing.
func (s *testAttestationStore) SaveAttestation(context.Context, *audit.WitnessAttestation) error {
	return s.Err
}

// Attestations records the bound it was asked for and answers Recorded and Err.
func (s *testAttestationStore) Attestations(_ context.Context, seq int64) ([]*audit.WitnessAttestation, error) {
	s.GotThrough = seq
	if s.Err != nil {
		return nil, s.Err
	}
	return s.Recorded, nil
}

// DeleteAttestation answers Err.
func (s *testAttestationStore) DeleteAttestation(context.Context, string) error { return s.Err }

// TestAttachStoredAttestationsReadsThroughTheHead pins how the callers reach the store: a store
// that keeps attestations is read up to the bundle's head and its matching records placed, a store
// that keeps none places none without failing, and a read that fails fails the attach with its
// cause.
func TestAttachStoredAttestationsReadsThroughTheHead(t *testing.T) {
	t.Parallel()
	errRead := errors.New("attestations unreadable")
	chain := treeChain(t, 6)
	producer := testProducerIdentity(t)
	w := testWitnessIdentity(t)
	shapes := testAttestedBundles{Chain: chain, Producer: producer}

	saved := audit.NewMemStore()
	held, ok := saved.(audit.AttestationStore)
	if !ok {
		t.Fatal("the memory store keeps no witness attestations")
	}
	for _, a := range []*audit.WitnessAttestation{
		testClaimAttestation(t, w, 2, chain[1].Hash),
		testClaimAttestation(t, w, 9, strings.Repeat("d", 64)),
	} {
		if err := held.SaveAttestation(context.Background(), a); err != nil {
			t.Fatalf("SaveAttestation() error = %v", err)
		}
	}

	tests := []struct {
		// Bundle is the bundle the attestations are read for, the whole linear chain when nil.
		Bundle *audit.Bundle
		// Store is the store the bundle reads from.
		Store audit.Store
		// Recorder is the store whose read bound is checked, nil when none is.
		Recorder *testAttestationStore
		// WantPlaced is the count AttachStoredAttestations reports.
		WantPlaced int
		// WantThrough is the bound the read was asked for.
		WantThrough int64
		// Want is the error the attach must wrap.
		Want error
	}{{ // Test 0: A store holding attestations places the one inside the bundle.
		Store: saved, WantPlaced: 1,
	}, { // Test 1: A store that keeps no attestations places none and does not fail.
		Store: struct{ audit.Store }{audit.NewMemStore()},
	}, { // Test 2: The read is bounded by the bundle's head.
		Recorder: &testAttestationStore{Store: audit.NewMemStore(),
			Recorded: []*audit.WitnessAttestation{testClaimAttestation(t, w, 6, chain[5].Hash)}},
		WantPlaced: 1, WantThrough: 6,
	}, { // Test 3: A read that fails fails the attach and names the cause.
		Recorder:    &testAttestationStore{Store: audit.NewMemStore(), Err: errRead},
		WantThrough: 6, Want: errRead,
	}, { // Test 4: A bundle with no chain reads every attestation and places none.
		Bundle: &audit.Bundle{},
		Recorder: &testAttestationStore{Store: audit.NewMemStore(), GotThrough: -1,
			Recorded: []*audit.WitnessAttestation{testClaimAttestation(t, w, 6, chain[5].Hash)}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := test.Store
			if test.Recorder != nil {
				store = test.Recorder
			}
			doc := test.Bundle
			if doc == nil {
				doc = shapes.Linear(t, chain)
			}
			got, err := doc.AttachStoredAttestations(context.Background(), store)
			if !errors.Is(err, test.Want) {
				t.Fatalf("AttachStoredAttestations() error = %v, want %v", err, test.Want)
			}
			if got != test.WantPlaced {
				t.Errorf("AttachStoredAttestations() = %d, want %d", got, test.WantPlaced)
			}
			if test.Recorder != nil && test.Recorder.GotThrough != test.WantThrough {
				t.Errorf("the read was bounded at seq %d, want %d", test.Recorder.GotThrough,
					test.WantThrough)
			}
		})
	}
}
