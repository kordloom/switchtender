package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/witness"
)

// witnessFixtureAt is when the fixture chain begins, and every later time is measured from it.
var witnessFixtureAt = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// witnessFixture is one producer's eight entry chain, a witness that countersigned it through seq
// 6, and a second witness and a second producer to hold it against.
type witnessFixture struct {
	// Producer signs the chain and every bundle.
	Producer identity.Identity
	// Other is a second producer, which no bundle here is signed by.
	Other identity.Identity
	// Witness countersigned the honest chain through seq 6.
	Witness identity.Identity
	// Stranger is a second witness key, which nobody here pinned.
	Stranger identity.Identity
	// Chain is the honest chain of eight entries under the producer's install.
	Chain []*audit.Entry
}

// newWitnessFixture builds the fixture, each identity in a directory of its own.
func newWitnessFixture(t *testing.T) *witnessFixture {
	t.Helper()
	load := func(witnessKey bool) identity.Identity {
		load := identity.LoadFile
		if witnessKey {
			load = identity.LoadWitnessFile
		}
		id, err := load(t.TempDir())
		if err != nil {
			t.Fatalf("load identity: %v", err)
		}
		return id
	}
	f := &witnessFixture{Producer: load(false), Other: load(false), Witness: load(true),
		Stranger: load(true)}
	f.Chain = witnessChain(f.Producer.InstallID, 8, 0)
	return f
}

// witnessChain builds a chain of n entries under installID. A rewriteAt above zero gives the entry
// at that seq a different path, and every link from there on follows, which is what a producer that
// rewrites its own history and re-signs it produces.
func witnessChain(installID string, n int, rewriteAt int64) []*audit.Entry {
	var chain []*audit.Entry
	var prev *audit.Entry
	for i := range n {
		path := fmt.Sprintf("/v1/templates/tpl_%d/launch", i)
		if int64(i+1) == rewriteAt {
			path = "/v1/templates/tpl_forged/launch"
		}
		e := &audit.Entry{
			ID: fmt.Sprintf("aud_%02d", i+1), At: witnessFixtureAt.Add(time.Duration(i) * time.Minute),
			Actor: "ops-token", ActorType: "token", Method: "POST", Path: path, InstallID: installID,
		}
		audit.Link(prev, e)
		chain = append(chain, e)
		prev = e
	}
	return chain
}

// attest has w countersign the entries of chain at seqs, the way a push witness answers a beat.
func (f *witnessFixture) attest(t *testing.T, w identity.Identity, chain []*audit.Entry,
	seqs ...int64) []*audit.WitnessAttestation {
	t.Helper()
	out := make([]*audit.WitnessAttestation, 0, len(seqs))
	for _, seq := range seqs {
		link := chain[seq-1].Hash
		cs, err := witness.ClaimAttestation(link, witnessFixtureAt.Add(time.Hour), w)
		if err != nil {
			t.Fatalf("witness.ClaimAttestation() error = %v", err)
		}
		out = append(out, &audit.WitnessAttestation{
			ID: audit.NewAttestationID(), Seq: seq, Link: link, Kind: audit.AttestationClaim,
			KeyID: cs.KeyID, PublicKey: cs.PublicKey, Role: cs.Role, At: cs.At, Sig: cs.Sig,
			Witness: cs.KeyID,
		})
	}
	return out
}

// bundle signs a linear bundle over entries with the producer key, carrying atts where they ride,
// writes it out, and returns its path.
func (f *witnessFixture) bundle(t *testing.T, entries []*audit.Entry,
	atts []*audit.WitnessAttestation) string {
	t.Helper()
	doc, err := audit.BuildBundle(entries, f.Producer, "1.106.0", witnessFixtureAt.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	doc.AttachAttestations(atts)
	signed, err := audit.SignBundleDoc(doc, f.Producer.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "audit.loomseal.json")
	if err := os.WriteFile(path, signed, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// statement is what the witness holds for the producer after countersigning the honest chain
// through seq 6, unsigned so a row can change it first.
func (f *witnessFixture) statement(t *testing.T) *witness.Statement {
	t.Helper()
	_, root, err := audit.TreeHead(f.Chain[:6], f.Producer.InstallID)
	if err != nil {
		t.Fatalf("TreeHead() error = %v", err)
	}
	stamp := witnessFixtureAt.Add(3 * time.Hour).Format(time.RFC3339)
	return &witness.Statement{
		InstallID: f.Producer.InstallID, KeyID: f.Producer.KeyID(),
		PublicKey: f.Producer.PublicKeyBase64(), MintedAt: stamp, CheckpointAt: stamp,
		LastBeat: 3, LastSeq: 6, LastHead: f.Chain[5].Hash, Size: 6, Root: root,
		MemorySince: witnessFixtureAt.Format(time.RFC3339), LastSeen: stamp, NotesTotal: 1,
	}
}

// writeStatement signs st with w and writes it out, returning its path.
func writeStatement(t *testing.T, st *witness.Statement, w identity.Identity) string {
	t.Helper()
	if err := witness.SignStatement(st, w); err != nil {
		t.Fatalf("SignStatement() error = %v", err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("json.Marshal(statement) error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "attestation.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// verifyWith runs verify over path with the producer pin, the witness pin, and the statement path,
// and returns the output and the error.
func verifyWith(t *testing.T, path, pubkey, witnessPubkey, statement string) (string, error) {
	t.Helper()
	verifyPubkey, verifyWitnessPubkey, verifyWitness = pubkey, witnessPubkey, statement
	t.Cleanup(func() { verifyPubkey, verifyWitnessPubkey, verifyWitness = "", "", "" })
	var buf bytes.Buffer
	c := testCommand()
	c.SetOut(&buf)
	err := runVerify(c, []string{path})
	return buf.String(), err
}

// verifiedLine matches the VERIFIED verdict line and nothing that merely contains the word.
var verifiedLine = regexp.MustCompile(`(?m)^VERIFIED`)

// witnessRow is one case of holding a bundle to a witness statement.
type witnessRow struct {
	// Name says what the case proves.
	Name string
	// Bundle builds the bundle and returns its path.
	Bundle func(t *testing.T, f *witnessFixture) string
	// Statement builds the statement and returns its path.
	Statement func(t *testing.T, f *witnessFixture) string
	// Unpinned leaves --witness-pubkey off.
	Unpinned bool
	// WantLines must all appear in the output, after each %W is replaced with the witness key id,
	// %S with the stranger's, %P with the producer's, and %O with the other producer's.
	WantLines []string
	// DenyWords must not appear in the output or the error.
	DenyWords []string
	// WantVerified is whether the VERIFIED verdict is printed and verify succeeds.
	WantVerified bool
}

// expand replaces the key placeholders a row's lines carry with the fixture's key ids.
func (f *witnessFixture) expand(s string) string {
	return strings.NewReplacer("%W", f.Witness.KeyID(), "%S", f.Stranger.KeyID(),
		"%P", f.Producer.KeyID(), "%O", f.Other.KeyID()).Replace(s)
}

// runWitnessRows runs each row against a fresh fixture.
func runWitnessRows(t *testing.T, rows []witnessRow) {
	t.Helper()
	for testNum, test := range rows {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: runVerify reads the package's flag variables.
			f := newWitnessFixture(t)
			path := test.Bundle(t, f)
			statement := ""
			if test.Statement != nil {
				statement = test.Statement(t, f)
			}
			pin := f.Witness.KeyID()
			if test.Unpinned {
				pin = ""
			}
			got, err := verifyWith(t, path, f.Producer.KeyID(), pin, statement)
			if (err == nil) != test.WantVerified {
				t.Fatalf("%s: runVerify() error = %v, want verified %t\n%s", test.Name, err,
					test.WantVerified, got)
			}
			if verifiedLine.MatchString(got) != test.WantVerified {
				t.Errorf("%s: VERIFIED printed = %t, want %t\n%s", test.Name, !test.WantVerified,
					test.WantVerified, got)
			}
			for _, want := range test.WantLines {
				if want = f.expand(want); !strings.Contains(got, want) {
					t.Errorf("%s: output is missing %q:\n%s", test.Name, want, got)
				}
			}
			for _, deny := range test.DenyWords {
				if strings.Contains(got, deny) || (err != nil && strings.Contains(err.Error(), deny)) {
					t.Errorf("%s: output or error says %q (error %v):\n%s", test.Name, deny, err,
						got)
				}
			}
		})
	}
}

// honestBundle is the whole honest chain with the witness's six countersignatures.
func honestBundle(t *testing.T, f *witnessFixture) string {
	t.Helper()
	return f.bundle(t, f.Chain, f.attest(t, f.Witness, f.Chain, 1, 2, 3, 4, 5, 6))
}

// honestStatement is the witness's statement for the honest chain, signed by the witness.
func honestStatement(t *testing.T, f *witnessFixture) string {
	t.Helper()
	return writeStatement(t, f.statement(t), f.Witness)
}

// statementWith returns a statement builder that edits the honest statement before the witness
// signs it.
func statementWith(
	edit func(f *witnessFixture, st *witness.Statement),
) func(*testing.T, *witnessFixture) string {
	return func(t *testing.T, f *witnessFixture) string {
		t.Helper()
		st := f.statement(t)
		edit(f, st)
		return writeStatement(t, st, f.Witness)
	}
}

// TestVerifyHoldsTheBundleToTheWitnessStatement pins that verify --witness fails a rewrite and
// passes an honest chain. A rewrite at or before a witnessed position changes the link there, so
// a bundle from the rewritten chain fails against the statement the witness still signs. A bundle
// that does not reach the witnessed position is not checkable and never VERIFIED, a refusal on the
// record fails it, and a note never does.
//
//nolint:funlen // Test function.
func TestVerifyHoldsTheBundleToTheWitnessStatement(t *testing.T) {
	runWitnessRows(t, []witnessRow{{ // Test 0: The honest chain holds and is verified.
		Name: "honest", Bundle: honestBundle, Statement: honestStatement, WantVerified: true,
		WantLines: []string{
			"chain        OK (8 entries recompute, head seq 8)",
			"witness      OK (6 beats countersigned by %W, newest seq 6, for producer %P, 1 note)",
			"The tree over its first 6 entries matches the root the witness holds.",
		},
		DenyWords: []string{"rewritten", "no witness key was pinned"},
	}, { // Test 1: A chain rewritten at seq 1 and re-signed fails at the witnessed seq.
		Name: "rewritten at seq 1",
		Bundle: func(t *testing.T, f *witnessFixture) string {
			return f.bundle(t, witnessChain(f.Producer.InstallID, 8, 1), nil)
		},
		Statement: honestStatement,
		WantLines: []string{
			"chain        OK (8 entries recompute, head seq 8)", "witness      FAILED (this bundle's " +
				"entry at seq 6 has link", "so the chain was rewritten at or before seq 6",
			"NOT VERIFIED: the witness statement does not hold",
		},
	}, { // Test 2: A chain rewritten at the witnessed seq fails there too.
		Name: "rewritten at last_seq",
		Bundle: func(t *testing.T, f *witnessFixture) string {
			return f.bundle(t, witnessChain(f.Producer.InstallID, 8, 6), nil)
		},
		Statement: honestStatement,
		WantLines: []string{"witness      FAILED (this bundle's entry at seq 6 has link"},
	}, { // Test 3: A window of the rewritten chain is caught by the link alone.
		Name: "rewritten window",
		Bundle: func(t *testing.T, f *witnessFixture) string {
			return f.bundle(t, witnessChain(f.Producer.InstallID, 8, 1)[2:], nil)
		},
		Statement: honestStatement,
		WantLines: []string{"witness      FAILED (this bundle's entry at seq 6 has link"},
	}, { // Test 4: A bundle that ends before the witnessed seq is not checkable and not VERIFIED.
		Name: "ends before last_seq",
		Bundle: func(t *testing.T, f *witnessFixture) string {
			return f.bundle(t, f.Chain[:5], f.attest(t, f.Witness, f.Chain, 1, 2, 3, 4, 5))
		},
		Statement: honestStatement,
		WantLines: []string{
			"witness      NOT CHECKABLE (this bundle ends at seq 5, before seq 6, the newest entry " +
				"the witness countersigned, so it is not checkable against this statement)",
			"NOT VERIFIED: this bundle is not checkable against the witness statement",
		},
		DenyWords: []string{"rewritten"},
	}, { // Test 5: Another install's statement names both producer keys and accuses nothing.
		Name: "another install's statement", Bundle: honestBundle,
		Statement: statementWith(func(f *witnessFixture, st *witness.Statement) {
			st.InstallID, st.KeyID, st.PublicKey = f.Other.InstallID, f.Other.KeyID(),
				f.Other.PublicKeyBase64()
		}),
		WantLines: []string{
			"chain        OK (8 entries recompute, head seq 8)",
			"witness      FAILED (witness statement is for key %O, bundle is signed by %P)",
		},
		DenyWords: []string{"rewritten"},
	}, { // Test 6: A refusal on the record fails the bundle, naming the count and the route.
		Name: "refusals_total 1", Bundle: honestBundle,
		Statement: statementWith(func(_ *witnessFixture, st *witness.Statement) {
			st.RefusalsTotal = 1
		}),
		WantLines: []string{"witness      FAILED (the witness refused 1 push from this install, " +
			"see GET /v1/installs/%P/findings)"},
	}, { // Test 7: Notes never fail the bundle and are counted on the OK line.
		Name: "notes_total 3, refusals_total 0", Bundle: honestBundle,
		Statement: statementWith(func(_ *witnessFixture, st *witness.Statement) {
			st.NotesTotal = 3
		}),
		WantVerified: true,
		WantLines: []string{"witness      OK (6 beats countersigned by %W, newest seq 6, for " +
			"producer %P, 3 notes)"},
	}, { // Test 8: Unpinned, the statement holds and the note says whose key went unchecked.
		Name: "unpinned", Bundle: honestBundle, Statement: honestStatement, Unpinned: true,
		WantVerified: true,
		WantLines: []string{"witness      OK (6 beats countersigned by %W", "no witness key was " +
			"pinned, so this says a witness signed, not which one."},
	}, { // Test 9: A statement of size 0 witnesses nothing and is never VERIFIED.
		Name: "size 0", Bundle: honestBundle,
		Statement: statementWith(func(_ *witnessFixture, st *witness.Statement) {
			st.LastBeat, st.LastSeq, st.LastHead, st.Size, st.Root, st.CheckpointAt = 0, 0, "", 0,
				"", ""
		}),
		WantLines: []string{"witness      NOT CHECKABLE (nothing witnessed: the witness signed " +
			"that it holds no countersigned head for install"},
	}, { // Test 10: A statement signed by a witness nobody pinned fails.
		Name: "statement by another witness", Bundle: honestBundle,
		Statement: func(t *testing.T, f *witnessFixture) string {
			return writeStatement(t, f.statement(t), f.Stranger)
		},
		WantLines: []string{"witness      FAILED (the witness statement is signed by %S, not the " +
			"pinned witness key %W)"},
	}, { // Test 11: A statement edited after it was signed fails its signature.
		Name: "edited statement", Bundle: honestBundle,
		Statement: func(t *testing.T, f *witnessFixture) string {
			st := f.statement(t)
			if err := witness.SignStatement(st, f.Witness); err != nil {
				t.Fatalf("SignStatement() error = %v", err)
			}
			st.RefusalsTotal = 0
			st.NotesTotal = 0
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			path := filepath.Join(t.TempDir(), "attestation.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			return path
		},
		WantLines: []string{"witness      FAILED (the witness statement does not verify"},
	}, { // Test 12: A window holding the witnessed seq is held to the link, and says so.
		Name: "honest window",
		Bundle: func(t *testing.T, f *witnessFixture) string {
			return f.bundle(t, f.Chain[2:], f.attest(t, f.Witness, f.Chain, 3, 4, 5, 6))
		},
		Statement: honestStatement, WantVerified: true,
		WantLines: []string{
			"witness      OK (4 beats countersigned by %W, newest seq 6, for producer %P, 1 note)",
			"the tree root was not compared, since this bundle does not carry every entry from " +
				"seq 1.",
		},
	}, { // Test 13: A root the bundle's tree does not reach fails, though the link matches.
		Name: "root the tree does not reach", Bundle: honestBundle,
		Statement: statementWith(func(f *witnessFixture, st *witness.Statement) {
			_, root, _ := audit.TreeHead(witnessChain(f.Producer.InstallID, 6, 2), f.Producer.InstallID)
			st.Root = root
		}),
		WantLines: []string{"witness      FAILED (the tree over this bundle's first 6 entries has " +
			"root"},
	}, { // Test 14: The same key under another label is a statement about another install.
		Name: "same key, another label", Bundle: honestBundle,
		Statement: statementWith(func(f *witnessFixture, st *witness.Statement) {
			st.InstallID = identity.LegacyInstallIDFromKey(f.Producer.Public())
		}),
		WantLines: []string{"witness      FAILED (the witness statement is about install in_"},
		DenyWords: []string{"rewritten"},
	}})
}

// TestVerifyWitnessHoldsTheStatementToTheKey pins that verify --witness compares the statement's
// producer key before its label. A statement minted for key B under the label the bundle names,
// and with the coordinate the bundle reproduces, is still the wrong statement: it fails naming both
// key ids, the chain line stays OK, and nothing says the chain was rewritten.
func TestVerifyWitnessHoldsTheStatementToTheKey(t *testing.T) {
	runWitnessRows(t, []witnessRow{{ // Test 0: Key B's statement under label X, held to A's bundle.
		Name: "statement for another key, same label", Bundle: honestBundle,
		Statement: statementWith(func(f *witnessFixture, st *witness.Statement) {
			st.KeyID, st.PublicKey = f.Other.KeyID(), f.Other.PublicKeyBase64()
		}),
		WantLines: []string{
			"chain        OK (8 entries recompute, head seq 8)",
			"witness      FAILED (witness statement is for key %O, bundle is signed by %P)",
			"NOT VERIFIED: the witness statement does not hold: witness statement is for key %O, " +
				"bundle is signed by %P",
		},
		DenyWords: []string{"rewritten"},
	}})
}

// TestVerifyCountsOnlyThePinnedWitness pins that a countersignature by a key other than the pinned
// witness is named and not counted, so a bundle cannot borrow a witness line from a key the relying
// party never trusted.
func TestVerifyCountsOnlyThePinnedWitness(t *testing.T) {
	mixed := func(t *testing.T, f *witnessFixture) string {
		t.Helper()
		atts := append(f.attest(t, f.Witness, f.Chain, 1, 2), f.attest(t, f.Stranger, f.Chain, 7)...)
		return f.bundle(t, f.Chain, atts)
	}
	strangerOnly := func(t *testing.T, f *witnessFixture) string {
		t.Helper()
		return f.bundle(t, f.Chain, f.attest(t, f.Stranger, f.Chain, 7, 8))
	}
	runWitnessRows(t, []witnessRow{{ // Test 0: Pinned, only the pinned witness counts.
		Name: "pinned", Bundle: mixed, WantVerified: true,
		WantLines: []string{
			"witness      OK (2 beats countersigned by %W, newest seq 2, for producer %P)",
			"  1 beat countersigned by %S, which is not the witness this bundle is held to, so not " +
				"counted",
		},
		DenyWords: []string{"3 beats", "no witness key was pinned"},
	}, { // Test 1: Unpinned, every witness that verifies counts, and the note says so.
		Name: "unpinned", Bundle: mixed, Unpinned: true, WantVerified: true,
		WantLines: []string{"witness      OK (3 beats countersigned by ", ", newest seq 7, for " +
			"producer %P)", "no witness key was pinned"},
		DenyWords: []string{"not counted"},
	}, { // Test 2: Pinned to a witness that signed nothing here, the line is none.
		Name: "only a stranger", Bundle: strangerOnly, WantVerified: true,
		WantLines: []string{
			"witness      none (the pinned witness %W countersigned nothing in this bundle)",
			"  2 beats countersigned by %S, which is not the witness this bundle is held to",
		},
		DenyWords: []string{"witness      OK"},
	}, { // Test 3: A bundle no witness countersigned says so.
		Name: "no witness",
		Bundle: func(t *testing.T, f *witnessFixture) string {
			return f.bundle(t, f.Chain, nil)
		},
		Unpinned: true, WantVerified: true,
		WantLines: []string{"witness      none (no outside witness countersigned this chain)"},
	}})
}

// TestAForgedCountersignatureFailsTheBundle pins that a witness attestation that does not verify
// over the entry it rides on fails the document, naming the entry, whoever signed it.
func TestAForgedCountersignatureFailsTheBundle(t *testing.T) {
	runWitnessRows(t, []witnessRow{{ // Test 0: The countersignature of seq 2 moved onto seq 3.
		Name: "moved countersignature",
		Bundle: func(t *testing.T, f *witnessFixture) string {
			t.Helper()
			doc, err := audit.BuildBundle(f.Chain, f.Producer, "1.106.0", witnessFixtureAt)
			if err != nil {
				t.Fatalf("BuildBundle() error = %v", err)
			}
			cs, err := witness.ClaimAttestation(f.Chain[1].Hash, witnessFixtureAt.Add(time.Hour),
				f.Witness)
			if err != nil {
				t.Fatalf("ClaimAttestation() error = %v", err)
			}
			raw, err := json.Marshal(cs)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			doc.Claims[2].Attestations = []json.RawMessage{raw}
			signed, err := audit.SignBundleDoc(doc, f.Producer.Private())
			if err != nil {
				t.Fatalf("SignBundleDoc() error = %v", err)
			}
			path := filepath.Join(t.TempDir(), "audit.loomseal.json")
			if err := os.WriteFile(path, signed, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			return path
		},
		WantLines: []string{
			"witness      FAILED (1 entry countersigned by a witness does not verify)",
			"the witness attestation by %W on the entry at seq 3 does not verify",
			"NOT VERIFIED: a witness countersignature does not verify over the entry it rides on",
		},
	}})
}
