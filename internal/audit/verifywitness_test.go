package audit_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/witness"
)

// testCountersignature has w countersign link and returns the attestation member a claim carries.
func testCountersignature(t *testing.T, w audit.Identity, link string) json.RawMessage {
	t.Helper()
	cs, err := witness.ClaimAttestation(link, testAttestAt, w)
	if err != nil {
		t.Fatalf("witness.ClaimAttestation() error = %v", err)
	}
	raw, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return raw
}

// TestVerifyBundleVerifiesWitnessAttestations pins that a claim attestation of role witness is
// verified under the LoomSeal rule and counted by key, that one that does not verify fails the
// document, and that every other attestation is still refused by name.
//
//nolint:funlen // Test function.
func TestVerifyBundleVerifiesWitnessAttestations(t *testing.T) {
	t.Parallel()
	producer := testProducerIdentity(t)
	chain := treeChainFor(t, 6, producer.InstallID)
	w, other := testWitnessIdentity(t), testWitnessIdentity(t)
	link := func(seq int64) string { return chain[seq-1].Hash }
	on := func(b *audit.Bundle, seq int64, raw ...json.RawMessage) {
		b.Claims[seq-1].Attestations = append(b.Claims[seq-1].Attestations, raw...)
	}
	tests := []struct {
		// Edit places attestations on the bundle before it is signed.
		Edit func(t *testing.T, b *audit.Bundle)
		// WantAttested is the WitnessAttested count.
		WantAttested int
		// WantKeys are the WitnessKeyIDs.
		WantKeys []string
		// WantNewest is the WitnessNewestSeq.
		WantNewest int64
		// WantProblems is how many WitnessProblems the report names.
		WantProblems int
		// WantOK is the report's verdict.
		WantOK bool
		// WantRefusal is a fragment of the refusal, empty when the bundle must be checked.
		WantRefusal string
		// Want is the error the refusal wraps.
		Want error
	}{{ // Test 0: No attestation counts nothing and verifies.
		Edit: func(*testing.T, *audit.Bundle) {}, WantOK: true,
	}, { // Test 1: Two countersignatures by one witness are counted under its key.
		Edit: func(t *testing.T, b *audit.Bundle) {
			on(b, 2, testCountersignature(t, w, link(2)))
			on(b, 5, testCountersignature(t, w, link(5)))
		},
		WantAttested: 2, WantKeys: []string{w.KeyID()}, WantNewest: 5, WantOK: true,
	}, { // Test 2: The same countersignature twice on one entry counts once.
		Edit: func(t *testing.T, b *audit.Bundle) {
			raw := testCountersignature(t, w, link(3))
			on(b, 3, raw, raw)
		},
		WantAttested: 1, WantKeys: []string{w.KeyID()}, WantNewest: 3, WantOK: true,
	}, { // Test 3: Two witnesses are both named and both counted.
		Edit: func(t *testing.T, b *audit.Bundle) {
			on(b, 1, testCountersignature(t, w, link(1)))
			on(b, 6, testCountersignature(t, other, link(6)))
		},
		WantAttested: 2, WantKeys: sortedKeys(w.KeyID(), other.KeyID()), WantNewest: 6,
		WantOK: true,
	}, { // Test 4: A countersignature moved to the next entry is a problem and fails the bundle.
		Edit: func(t *testing.T, b *audit.Bundle) {
			on(b, 3, testCountersignature(t, w, link(2)))
		},
		WantProblems: 1,
	}, { // Test 5: A countersignature whose key id names another key fails.
		Edit: func(t *testing.T, b *audit.Bundle) {
			var cs witness.Countersignature
			if err := json.Unmarshal(testCountersignature(t, w, link(4)), &cs); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			cs.KeyID = other.KeyID()
			raw, err := json.Marshal(cs)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			on(b, 4, raw)
		},
		WantProblems: 1,
	}, { // Test 6: An attestation of another role is refused by name.
		Edit: func(t *testing.T, b *audit.Bundle) {
			on(b, 2, json.RawMessage(strings.Replace(string(testCountersignature(t, w, link(2))),
				`"role":"witness"`, `"role":"auditor"`, 1)))
		},
		WantRefusal: `claim 1 carries an attestation of role "auditor"`, Want: audit.ErrVerify,
	}, { // Test 7: A case variant of a member is refused rather than folded.
		Edit: func(t *testing.T, b *audit.Bundle) {
			on(b, 2, json.RawMessage(strings.Replace(string(testCountersignature(t, w, link(2))),
				`"role":"witness"`, `"role":"witness","Role":"auditor"`, 1)))
		},
		WantRefusal: `member "Role" is not an attestation member`, Want: audit.ErrVerify,
	}, { // Test 8: A member that is not a string is refused.
		Edit: func(_ *testing.T, b *audit.Bundle) {
			on(b, 2, json.RawMessage(`{"role":"witness","sig":7}`))
		},
		WantRefusal: `member "sig" is not a string`, Want: audit.ErrVerify,
	}, { // Test 9: A head attestation is still refused by name.
		Edit: func(t *testing.T, b *audit.Bundle) {
			b.Attestations = []json.RawMessage{testCountersignature(t, w, link(6))}
		},
		WantRefusal: "bundle carries attestations: this product does not verify them",
		Want:        audit.ErrVerify,
	}, { // Test 10: A disclosure is still refused by name.
		Edit: func(_ *testing.T, b *audit.Bundle) {
			b.Claims[0].Disclosures = []json.RawMessage{json.RawMessage(`{}`)}
		},
		WantRefusal: "claim 0 carries disclosures", Want: audit.ErrVerify,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc, err := audit.BuildBundle(chain, producer, "1.106.0", testAttestAt)
			if err != nil {
				t.Fatalf("BuildBundle() error = %v", err)
			}
			test.Edit(t, doc)
			rep, err := audit.VerifyBundle(signBundle(t, doc, producer), producer.KeyID())
			if !errors.Is(err, test.Want) {
				t.Fatalf("VerifyBundle() error = %v, want %v", err, test.Want)
			}
			if test.WantRefusal != "" {
				if !strings.Contains(err.Error(), test.WantRefusal) {
					t.Errorf("VerifyBundle() error = %q, want it to say %q", err, test.WantRefusal)
				}
				return
			}
			if rep.WitnessAttested != test.WantAttested || rep.WitnessNewestSeq != test.WantNewest {
				t.Errorf("WitnessAttested, WitnessNewestSeq = %d, %d, want %d, %d",
					rep.WitnessAttested, rep.WitnessNewestSeq, test.WantAttested, test.WantNewest)
			}
			if diff := cmp.Diff(test.WantKeys, rep.WitnessKeyIDs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("WitnessKeyIDs mismatch (-want +got):\n%s", diff)
			}
			if len(rep.WitnessProblems) != test.WantProblems {
				t.Errorf("WitnessProblems = %q, want %d", rep.WitnessProblems, test.WantProblems)
			}
			if rep.OK() != test.WantOK {
				t.Errorf("OK() = %t, want %t", rep.OK(), test.WantOK)
			}
			if !rep.ChainOK || !rep.SignatureOK {
				t.Errorf("chain %t, signature %t: a witness attestation changed the chain verdict",
					rep.ChainOK, rep.SignatureOK)
			}
		})
	}
}

// sortedKeys returns the two key ids in the order a report lists them.
func sortedKeys(a, b string) []string {
	if b < a {
		return []string{b, a}
	}
	return []string{a, b}
}

// TestCheckWitnessStatementStates pins each verdict CheckWitnessStatement reaches and why, over one
// producer's eight entry chain that a witness countersigned through seq 6.
//
//nolint:funlen // Test function.
func TestCheckWitnessStatementStates(t *testing.T) {
	t.Parallel()
	producer, other := testProducerIdentity(t), testProducerIdentity(t)
	w, stranger := testWitnessIdentity(t), testWitnessIdentity(t)
	chain := treeChainFor(t, 8, producer.InstallID)
	rewritten := treeChainFor(t, 8, producer.InstallID)
	rewritten[0].Path = "/v1/runs/forged"
	for i := range rewritten {
		var prev *audit.Entry
		if i > 0 {
			prev = rewritten[i-1]
		}
		audit.Link(prev, rewritten[i])
	}
	_, root, err := audit.TreeHead(chain[:6], producer.InstallID)
	if err != nil {
		t.Fatalf("TreeHead() error = %v", err)
	}
	stamp := testAttestAt.Format(time.RFC3339)
	honest := func() *witness.Statement {
		return &witness.Statement{
			InstallID: producer.InstallID, KeyID: producer.KeyID(),
			PublicKey: producer.PublicKeyBase64(), MintedAt: stamp, CheckpointAt: stamp,
			LastBeat: 3, LastSeq: 6, LastHead: chain[5].Hash, Size: 6, Root: root,
			MemorySince: stamp, LastSeen: stamp, NotesTotal: 2,
		}
	}
	bundle := func(t *testing.T, entries []*audit.Entry) []byte {
		t.Helper()
		doc, err := audit.BuildBundle(entries, producer, "1.106.0", testAttestAt)
		if err != nil {
			t.Fatalf("BuildBundle() error = %v", err)
		}
		return signBundle(t, doc, producer)
	}
	tests := []struct {
		// Entries are the entries the bundle carries, the whole honest chain when nil.
		Entries []*audit.Entry
		// Edit changes the honest statement before it is signed.
		Edit func(st *witness.Statement)
		// Signer signs the statement, the pinned witness when nil.
		Signer audit.Identity
		// Pin is the witness pin, the pinned witness's key id when empty and Unpinned is false.
		Pin string
		// Unpinned leaves the pin off.
		Unpinned bool
		// WantState is the verdict.
		WantState audit.StatementState
		// WantProblem is a fragment of the problem, empty when the statement holds.
		WantProblem string
		// WantRootCompared is whether the root was compared.
		WantRootCompared bool
		// WantPinned is whether the signer matched a pin.
		WantPinned bool
	}{{ // Test 0: The honest chain holds, its root compared.
		WantState: audit.StatementHeld, WantRootCompared: true, WantPinned: true,
	}, { // Test 1: The raw hex public key is the same pin as the key id.
		Pin: hex.EncodeToString(w.Public()), WantState: audit.StatementHeld,
		WantRootCompared: true, WantPinned: true,
	}, { // Test 2: Unpinned, the statement holds and says it was not pinned.
		Unpinned: true, WantState: audit.StatementHeld, WantRootCompared: true,
	}, { // Test 3: A window that holds seq 6 is held to its link alone.
		Entries: chain[3:], WantState: audit.StatementHeld, WantPinned: true,
	}, { // Test 4: A tree size the bundle does not reach leaves the root uncompared.
		Edit:      func(st *witness.Statement) { st.Size = 1 << 40 },
		WantState: audit.StatementHeld, WantPinned: true,
	}, { // Test 5: The rewritten chain fails on the link at seq 6.
		Entries: rewritten, WantState: audit.StatementFailed,
		WantProblem: "entry at seq 6 has link",
	}, { // Test 6: A window of the rewritten chain fails on the link alone.
		Entries: rewritten[3:], WantState: audit.StatementFailed,
		WantProblem: "so the chain was rewritten at or before seq 6",
	}, { // Test 7: A root the bundle's tree does not reach fails.
		Edit: func(st *witness.Statement) {
			_, forged, _ := audit.TreeHead(rewritten[:6], producer.InstallID)
			st.Root = forged
		},
		WantState: audit.StatementFailed, WantProblem: "the tree over this bundle's first 6 entries",
	}, { // Test 8: A bundle that ends before seq 6 is not checkable.
		Entries: chain[:4], WantState: audit.StatementNotCheckable,
		WantProblem: "this bundle ends at seq 4, before seq 6",
	}, { // Test 9: A size 0 statement witnesses nothing.
		Edit: func(st *witness.Statement) {
			st.LastBeat, st.LastSeq, st.LastHead, st.Size, st.Root = 0, 0, "", 0, ""
		},
		WantState: audit.StatementNothingWitnessed, WantProblem: "nothing witnessed",
	}, { // Test 10: A size 0 statement that names a beat is out of form.
		Edit: func(st *witness.Statement) {
			st.Size, st.Root = 0, ""
		},
		WantState: audit.StatementFailed, WantProblem: "holds a tree of 0 entries and names beat 3",
	}, { // Test 11: A refusal on the record fails, ahead of the coordinate.
		Entries: chain[:4], Edit: func(st *witness.Statement) { st.RefusalsTotal = 2 },
		WantState: audit.StatementFailed, WantProblem: "the witness refused 2 pushes",
	}, { // Test 12: A statement for another key with this label fails naming both keys.
		Edit: func(st *witness.Statement) {
			st.KeyID, st.PublicKey = other.KeyID(), other.PublicKeyBase64()
		},
		WantState: audit.StatementFailed,
		WantProblem: "witness statement is for key " + other.KeyID() + ", bundle is signed by " +
			producer.KeyID(),
	}, { // Test 13: A statement for this key under another label fails on the label.
		Edit:      func(st *witness.Statement) { st.InstallID = other.InstallID },
		WantState: audit.StatementFailed, WantProblem: "the witness statement is about install",
	}, { // Test 14: A statement by a key nobody pinned fails.
		Signer: stranger, WantState: audit.StatementFailed,
		WantProblem: "not the pinned witness key " + w.KeyID(),
	}, { // Test 15: A pin in neither form fails.
		Pin: "sha256:nothex", WantState: audit.StatementFailed, WantProblem: "witness key pin",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			entries := test.Entries
			if entries == nil {
				entries = chain
			}
			signed := bundle(t, entries)
			rep, err := audit.VerifyBundle(signed, producer.KeyID())
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			st := honest()
			if test.Edit != nil {
				test.Edit(st)
			}
			signer := w
			if test.Signer.Private() != nil {
				signer = test.Signer
			}
			if err := witness.SignStatement(st, signer); err != nil {
				t.Fatalf("SignStatement() error = %v", err)
			}
			pin := test.Pin
			if pin == "" && !test.Unpinned {
				pin = w.KeyID()
			}
			got := audit.CheckWitnessStatement(rep, signed, st, pin)
			if got.State != test.WantState {
				t.Fatalf("State = %q (%s), want %q", got.State, got.Problem, test.WantState)
			}
			if got.OK() != (test.WantState == audit.StatementHeld) {
				t.Errorf("OK() = %t in state %q", got.OK(), got.State)
			}
			if test.WantProblem == "" && got.Problem != "" {
				t.Errorf("Problem = %q, want none", got.Problem)
			}
			if !strings.Contains(got.Problem, test.WantProblem) {
				t.Errorf("Problem = %q, want it to say %q", got.Problem, test.WantProblem)
			}
			if strings.Contains(got.Problem, "rewritten") && test.WantState != audit.StatementFailed {
				t.Errorf("Problem = %q accuses a chain it could not check", got.Problem)
			}
			if test.WantState == audit.StatementHeld &&
				(got.RootCompared != test.WantRootCompared || got.Pinned != test.WantPinned) {
				t.Errorf("RootCompared, Pinned = %t, %t, want %t, %t", got.RootCompared, got.Pinned,
					test.WantRootCompared, test.WantPinned)
			}
		})
	}
}

// TestCheckWitnessStatementRefusesNothingToCheck pins the inputs a statement check cannot read: no
// statement, and no verified bundle.
func TestCheckWitnessStatementRefusesNothingToCheck(t *testing.T) {
	t.Parallel()
	w := testWitnessIdentity(t)
	st := &witness.Statement{InstallID: "in_x", KeyID: w.KeyID(), PublicKey: w.PublicKeyBase64()}
	if err := witness.SignStatement(st, w); err != nil {
		t.Fatalf("SignStatement() error = %v", err)
	}
	tests := []struct {
		// Statement is the statement held to.
		Statement *witness.Statement
		// WantProblem is a fragment of the problem.
		WantProblem string
	}{{ // Test 0: No statement.
		WantProblem: "no witness statement was given",
	}, { // Test 1: No verified bundle.
		Statement: st, WantProblem: "the bundle was not verified",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := audit.CheckWitnessStatement(nil, nil, test.Statement, "")
			if got.State != audit.StatementFailed || !strings.Contains(got.Problem, test.WantProblem) {
				t.Errorf("CheckWitnessStatement() = %q (%s), want failed saying %q", got.State,
					got.Problem, test.WantProblem)
			}
		})
	}
}

// TestWitnessPinKeyID pins the two forms a witness key pin may take and the refusal of any other.
func TestWitnessPinKeyID(t *testing.T) {
	t.Parallel()
	w := testWitnessIdentity(t)
	hexKey := hex.EncodeToString(w.Public())
	tests := []struct {
		// In is the pin.
		In string
		// WantID is the key id it names.
		WantID string
		// Want is the error.
		Want error
	}{{ // Test 0: The key id names itself.
		In: w.KeyID(), WantID: w.KeyID(),
	}, { // Test 1: The key id in capitals names the same key.
		In: "sha256:" + strings.ToUpper(strings.TrimPrefix(w.KeyID(), "sha256:")), WantID: w.KeyID(),
	}, { // Test 2: The raw hex public key names its key id.
		In: hexKey, WantID: w.KeyID(),
	}, { // Test 3: Surrounding space is not part of the pin.
		In: " " + hexKey + "\n", WantID: w.KeyID(),
	}, { // Test 4: Nothing is no key.
		In: "", Want: audit.ErrVerify,
	}, { // Test 5: A short key id is no key.
		In: "sha256:abcd", Want: audit.ErrVerify,
	}, { // Test 6: A base64 key is not a published form.
		In: w.PublicKeyBase64(), Want: audit.ErrVerify,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := audit.WitnessPinKeyID(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("WitnessPinKeyID(%q) error = %v, want %v", test.In, err, test.Want)
			}
			if diff := cmp.Diff(test.WantID, got); diff != "" {
				t.Errorf("WitnessPinKeyID(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}
