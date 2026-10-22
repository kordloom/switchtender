package witness

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/merkle"

	"github.com/kordloom/switchtender/identity"
)

// testInstall is the install the tree tests push for.
const testInstall = "in_0123456789abcdef0123456789abcdef"

// testLeaves returns n leaf preimages in the shape the chain's tree profile hashes: a canonical
// object naming the profile, the install, and a digest of one entry's content. The entries named in
// rewritten carry other content, so a rewrite is a real edit of real leaves and every root and
// proof built over them is the one the merkle package computes.
func testLeaves(t *testing.T, n int64, rewritten ...int64) [][]byte {
	t.Helper()
	leaves := make([][]byte, 0, n)
	for i := int64(1); i <= n; i++ {
		content := fmt.Sprintf("entry %d", i)
		if slices.Contains(rewritten, i) {
			content = fmt.Sprintf("rewritten entry %d", i)
		}
		leaf, err := jcs.Serialize(map[string]any{
			"domain":     "loomseal-merkle-v1",
			"install_id": testInstall,
			"claim":      "sha256:" + merkle.Sum256Hex([]byte(content)),
		})
		if err != nil {
			t.Fatalf("Serialize() error = %v", err)
		}
		leaves = append(leaves, leaf)
	}
	return leaves
}

// testRoot returns the hex tree root over the first size leaves.
func testRoot(leaves [][]byte, size int64) string {
	return hex.EncodeToString(merkle.Root(leaves[:size]))
}

// testHead returns the link a hash chain over the leaves carries at position seq, so a rewrite of
// any entry changes the link at every position after it, as it does in the audit chain.
func testHead(leaves [][]byte, seq int64) string {
	link := ""
	for _, leaf := range leaves[:seq] {
		link = merkle.Sum256Hex(append([]byte(link), leaf...))
	}
	return link
}

// testProof returns the hex consistency proof from the tree at from to the tree at to over leaves,
// empty when from is zero.
func testProof(t *testing.T, leaves [][]byte, from, to int64) []string {
	t.Helper()
	if from == 0 {
		return nil
	}
	proof, err := merkle.ConsistencyProof(from, leaves[:to])
	if err != nil {
		t.Fatalf("ConsistencyProof(%d, %d) error = %v", from, to, err)
	}
	return hexHashes(proof)
}

// hexHashes encodes hashes as hex strings, the form a push carries them in.
func hexHashes(in [][]byte) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		out = append(out, hex.EncodeToString(h))
	}
	return out
}

// testPush returns the push the chain over leaves makes for beat at size, proving from from.
func testPush(t *testing.T, leaves [][]byte, beat, size, from int64) Push {
	t.Helper()
	return Push{Switchtender: PushVersion, InstallID: testInstall, BeatNumber: beat,
		At: "2026-10-21T00:00:00Z", Seq: size, Head: testHead(leaves, size), Size: size,
		Root: testRoot(leaves, size), FromSize: from, Proof: testProof(t, leaves, from, size)}
}

// witnessedAt returns the memory a witness holds after the chain over leaves pushed beats 1, 2,
// and so on at each of sizes, each proving from the size before it.
func witnessedAt(t *testing.T, leaves [][]byte, now time.Time, sizes ...int64) *Checkpoint {
	t.Helper()
	var cp *Checkpoint
	from := int64(0)
	for i, size := range sizes {
		p := testPush(t, leaves, int64(i+1), size, from)
		next, _, err := CheckPush(cp, testInstall, p, now)
		if err != nil {
			t.Fatalf("CheckPush(beat %d at size %d) error = %v", i+1, size, err)
		}
		if !holdsPush(next, &p) {
			t.Fatalf("the honest push of beat %d at size %d was not adopted", i+1, size)
		}
		cp, from = next, size
	}
	return cp
}

// signedBytes returns a copy of c signed by id and encoded, which is the memory as the witness
// would save it, so two memories compare byte for byte.
func signedBytes(t *testing.T, c *Checkpoint, id identity.Identity) []byte {
	t.Helper()
	cp := *c
	if err := Sign(&cp, id); err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	raw, err := json.Marshal(&cp)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return raw
}

// TestPushRefusesAChainThatDoesNotExtendTheWitnessedRoot is design note invariant 1: no push is
// countersigned unless its proof extends the witnessed root. The witness holds the honest chain at
// size 5 and every row pushes beat 3 at size 8, from a chain rewritten at the first entry, at the
// last witnessed entry, or one past it, carrying the best proof each attacker can build. A rewrite
// one past the witnessed size is adopted, because no witness can refuse an entry it never saw, and
// that row is what shows the refusal sits exactly at the witnessed boundary.
func TestPushRefusesAChainThatDoesNotExtendTheWitnessedRoot(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	const witnessed, pushed = int64(5), int64(8)
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	id := testIdentity(t)
	honest := testLeaves(t, pushed)
	tests := []struct {
		Push        func(t *testing.T) Push
		WantKinds   []string
		WantAdopted bool
	}{{ // Test 0: The honest chain extends what was witnessed and is adopted.
		Push:        func(t *testing.T) Push { return testPush(t, honest, 3, pushed, witnessed) },
		WantAdopted: true,
	}, { // Test 1: Rewritten at the first entry, proved over the rewritten chain.
		Push: func(t *testing.T) Push {
			return testPush(t, testLeaves(t, pushed, 1), 3, pushed, witnessed)
		},
		WantKinds: []string{"rewritten_history"},
	}, { // Test 2: Rewritten at the last witnessed entry, proved over the rewritten chain.
		Push: func(t *testing.T) Push {
			return testPush(t, testLeaves(t, pushed, witnessed), 3, pushed, witnessed)
		},
		WantKinds: []string{"rewritten_history"},
	}, { // Test 3: Rewritten one past the last witnessed entry, which this witness never saw.
		Push: func(t *testing.T) Push {
			return testPush(t, testLeaves(t, pushed, witnessed+1), 3, pushed, witnessed)
		},
		WantAdopted: true,
	}, { // Test 4: Rewritten at the first entry, carrying the honest chain's proof.
		Push: func(t *testing.T) Push {
			p := testPush(t, testLeaves(t, pushed, 1), 3, pushed, witnessed)
			p.Proof = testProof(t, honest, witnessed, pushed)
			return p
		},
		WantKinds: []string{"rewritten_history"},
	}, { // Test 5: Rewritten at the last witnessed entry, carrying the honest chain's proof.
		Push: func(t *testing.T) Push {
			p := testPush(t, testLeaves(t, pushed, witnessed), 3, pushed, witnessed)
			p.Proof = testProof(t, honest, witnessed, pushed)
			return p
		},
		WantKinds: []string{"rewritten_history"},
	}, { // Test 6: The honest head and root with no proof, the head-only push.
		Push: func(t *testing.T) Push {
			p := testPush(t, honest, 3, pushed, witnessed)
			p.Proof = nil
			return p
		},
		WantKinds: []string{"rewritten_history"},
	}, { // Test 7: The honest chain proving from a size one short of the held one.
		Push: func(t *testing.T) Push {
			p := testPush(t, honest, 3, pushed, witnessed)
			p.Proof = testProof(t, honest, witnessed-1, pushed)
			return p
		},
		WantKinds: []string{"rewritten_history"},
	}, { // Test 8: A fork at the witnessed size itself, with nothing appended.
		Push: func(t *testing.T) Push {
			return testPush(t, testLeaves(t, pushed, 3), 3, witnessed, witnessed)
		},
		WantKinds: []string{"rewritten_history", "rewritten_history"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			prev := witnessedAt(t, honest, now, 2, witnessed)
			before := signedBytes(t, prev, id)
			p := test.Push(t)
			next, findings, err := CheckPush(prev, testInstall, p, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("CheckPush() error = %v", err)
			}
			if diff := cmp.Diff(test.WantKinds, kindsOf(findings), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("finding kinds mismatch (-want +got):\n%s", diff)
			}
			if got := holdsPush(next, &p); got != test.WantAdopted {
				t.Errorf("adopted = %v, want %v; checkpoint holds size %d root %s", got,
					test.WantAdopted, next.Size, next.Root)
			}
			if !test.WantAdopted && string(signedBytes(t, next, id)) != string(before) {
				t.Errorf("a refused push changed the memory the witness holds")
			}
			if string(signedBytes(t, prev, id)) != string(before) {
				t.Errorf("CheckPush() edited the previous checkpoint")
			}
			// The refusal names both trees, so an operator can see which coordinate was witnessed
			// and which the push claimed.
			for _, f := range findings {
				if f.Key != fmt.Sprintf("rewritten_history from witnessed tree size %d", witnessed) {
					continue
				}
				for _, want := range []string{fmt.Sprintf("size %d", p.Size), p.Root,
					fmt.Sprintf("size %d", witnessed), prev.Root} {
					if !strings.Contains(f.Detail, want) {
						t.Errorf("rewritten_history detail %q does not name %s", f.Detail, want)
					}
				}
			}
		})
	}
}

// TestAPushBehindTheHeldSizeIsStale is design note invariant 21: a push behind the held size is
// stale, not a rewrite, and a cut chain is refused once it reaches the witnessed size. The witness
// holds the honest chain at size 5. A push proving from there while claiming a smaller tree is
// ErrStale naming both sizes, and nothing else, because one push cannot tell a replica's superseded
// beat from a chain cut back. The cut chain grown back to the witnessed size with other entries,
// or past it, is rewritten_history, and the honest chain's next push is adopted from the same
// memory.
func TestAPushBehindTheHeldSizeIsStale(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	const witnessed = int64(5)
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	id := testIdentity(t)
	honest := testLeaves(t, 8)
	regrown := testLeaves(t, 8, witnessed)
	tests := []struct {
		Push        func(t *testing.T) Push
		Want        error
		WantText    []string
		WantKinds   []string
		WantAdopted bool
	}{{ // Test 0: Cut back to one short of the witnessed size, proving from the witnessed size.
		Push: func(t *testing.T) Push {
			p := testPush(t, honest, 3, witnessed-1, 0)
			p.FromSize = witnessed
			return p
		},
		Want:     ErrStale,
		WantText: []string{"holds the tree at size 5", "claims size 4"},
	}, { // Test 1: Cut back to size one, proving from the witnessed size.
		Push: func(t *testing.T) Push {
			p := testPush(t, honest, 3, 1, 0)
			p.FromSize = witnessed
			return p
		},
		Want:     ErrStale,
		WantText: []string{"holds the tree at size 5", "claims size 1"},
	}, { // Test 2: Cut back, proving from a size this witness does not hold.
		Push: func(t *testing.T) Push { return testPush(t, honest, 3, witnessed-1, 3) },
		Want: ErrProofBase, WantText: []string{"holds the tree at size 5", "proves from size 3"},
	}, { // Test 3: The cut chain grown back to the witnessed size with another last entry.
		Push: func(t *testing.T) Push {
			return testPush(t, regrown, 3, witnessed, witnessed)
		},
		WantKinds: []string{"rewritten_history", "rewritten_history"},
	}, { // Test 4: The cut chain grown past the witnessed size with another last witnessed entry.
		Push: func(t *testing.T) Push {
			return testPush(t, regrown, 3, witnessed+2, witnessed)
		},
		WantKinds: []string{"rewritten_history"},
	}, { // Test 5: The honest chain's next push.
		Push: func(t *testing.T) Push {
			return testPush(t, honest, 3, witnessed+2, witnessed)
		},
		WantAdopted: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			prev := witnessedAt(t, honest, now, 2, witnessed)
			before := signedBytes(t, prev, id)
			p := test.Push(t)
			next, findings, err := CheckPush(prev, testInstall, p, now.Add(time.Minute))
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Fatalf("CheckPush() = %v, %v, %v; want error %v", next, findings, err, test.Want)
			}
			if string(signedBytes(t, prev, id)) != string(before) {
				t.Errorf("CheckPush() edited the previous checkpoint")
			}
			if test.Want != nil {
				if next != nil || findings != nil {
					t.Errorf("CheckPush() = %v, %v with error %v; want the error alone", next,
						findings, err)
				}
				for _, want := range test.WantText {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("CheckPush() error = %q, want it to say %q", err, want)
					}
				}
				return
			}
			if diff := cmp.Diff(test.WantKinds, kindsOf(findings), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("finding kinds mismatch (-want +got):\n%s", diff)
			}
			if got := holdsPush(next, &p); got != test.WantAdopted {
				t.Errorf("adopted = %v, want %v; checkpoint holds size %d root %s", got,
					test.WantAdopted, next.Size, next.Root)
			}
			if !test.WantAdopted && string(signedBytes(t, next, id)) != string(before) {
				t.Errorf("a refused push changed the memory the witness holds")
			}
		})
	}
}

// TestEveryRewriteAtOrBeforeTheWitnessedSizeIsRefused sweeps the boundary of invariant 1 across
// tree shapes: for every witnessed size up to twelve, every growth up to three, and every entry in
// the grown chain, a rewrite at or before the witnessed size is refused and a rewrite after it is
// adopted. Odd sizes, powers of two, and their neighbors each fold a proof differently.
func TestEveryRewriteAtOrBeforeTheWitnessedSizeIsRefused(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for witnessed := int64(1); witnessed <= 12; witnessed++ {
		honest := testLeaves(t, witnessed+3)
		prev := witnessedAt(t, honest, now, witnessed)
		for grow := int64(1); grow <= 3; grow++ {
			for at := int64(1); at <= witnessed+grow; at++ {
				p := testPush(t, testLeaves(t, witnessed+grow, at), 2, witnessed+grow, witnessed)
				next, findings, err := CheckPush(prev, testInstall, p, now)
				if err != nil {
					t.Fatalf("witnessed %d, grown by %d, rewritten at %d: error = %v", witnessed,
						grow, at, err)
				}
				refused := at <= witnessed
				if got := !holdsPush(next, &p); got != refused {
					t.Errorf("witnessed %d, grown by %d, rewritten at %d: refused = %v, want %v",
						witnessed, grow, at, got, refused)
				}
				if refused != slices.Contains(kindsOf(findings), "rewritten_history") {
					t.Errorf("witnessed %d, grown by %d, rewritten at %d: findings %v", witnessed,
						grow, at, findings)
				}
			}
		}
	}
}

// TestARewrittenChainStaysRefused is design note invariant 2. After a rewrite at a witnessed entry
// is refused, three more pushes built on the rewritten chain are each refused, and the memory the
// witness would sign is byte-identical throughout. Claiming the refused push was acknowledged is
// answered with the held size, and the honest chain's next push is still adopted from the same
// memory, so the attack cost the honest chain nothing.
func TestARewrittenChainStaysRefused(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	id := testIdentity(t)
	honest := testLeaves(t, 12)
	forged := testLeaves(t, 12, 4)
	cp := witnessedAt(t, honest, now, 3, 5)
	before := signedBytes(t, cp, id)
	for i, size := range []int64{6, 7, 8, 9} {
		p := testPush(t, forged, int64(3+i), size, 5)
		next, findings, err := CheckPush(cp, testInstall, p, now.Add(time.Duration(i+1)*time.Minute))
		if err != nil {
			t.Fatalf("push %d: CheckPush() error = %v", i, err)
		}
		if !slices.Contains(kindsOf(findings), "rewritten_history") || holdsPush(next, &p) {
			t.Errorf("push %d on the rewritten chain: findings %v, adopted %v; want refused", i,
				findings, holdsPush(next, &p))
		}
		if string(signedBytes(t, next, id)) != string(before) {
			t.Errorf("push %d on the rewritten chain changed the memory the witness signs", i)
		}
		cp = next
	}

	// The forger claims its first push was acknowledged and proves from there.
	claimed := testPush(t, forged, 7, 10, 6)
	next, findings, err := CheckPush(cp, testInstall, claimed, now.Add(time.Hour))
	if !errors.Is(err, ErrProofBase) || next != nil || findings != nil {
		t.Fatalf("CheckPush() from an unacknowledged base = %v, %v, %v; want %v alone", next,
			findings, err, ErrProofBase)
	}
	if !strings.Contains(err.Error(), "size 5") {
		t.Errorf("CheckPush() error = %q, want it to name the held size 5", err)
	}

	honestNext := testPush(t, honest, 3, 6, 5)
	adopted, findings, err := CheckPush(cp, testInstall, honestNext, now.Add(2*time.Hour))
	if err != nil || len(findings) != 0 || !holdsPush(adopted, &honestNext) {
		t.Errorf("the honest chain's next push = %v, %v, adopted %v; want it adopted cleanly",
			findings, err, holdsPush(adopted, &honestNext))
	}
}

// TestARetriedPushReturnsTheSameAttestation is design note invariant 12. A push retried byte for
// byte after it was adopted raises nothing and adopts nothing, however long after it comes and
// although the base it proves from is no longer the held size, and the attestations the witness
// mints from the memory it holds are byte-equal to the first answer's.
func TestARetriedPushReturnsTheSameAttestation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	id := testIdentity(t)
	honest := testLeaves(t, 6)
	prev := witnessedAt(t, honest, now, 3)
	p := testPush(t, honest, 2, 6, 3)
	first, findings, err := CheckPush(prev, testInstall, p, now.Add(time.Minute))
	if err != nil || len(findings) != 0 || !holdsPush(first, &p) {
		t.Fatalf("CheckPush() = %v, %v, adopted %v; want the push adopted", findings, err,
			holdsPush(first, &p))
	}
	firstAnswer := mintAnswer(t, first, id)
	tests := []struct {
		After time.Duration
	}{{ // Test 0: Retried at once.
		After: time.Second,
	}, { // Test 1: Retried an hour later.
		After: time.Hour,
	}, { // Test 2: Retried after an outage longer than the stall bound.
		After: StaleAfter + time.Hour,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			retried, findings, err := CheckPush(first, testInstall, p,
				first.ObservedAt.Add(test.After))
			if err != nil || len(findings) != 0 {
				t.Fatalf("CheckPush() of a retry = %v, %v; want nothing raised", findings, err)
			}
			if diff := cmp.Diff(string(signedBytes(t, first, id)),
				string(signedBytes(t, retried, id))); diff != "" {
				t.Errorf("a retry changed the memory (-first +retry):\n%s", diff)
			}
			if len(retried.Recent) != len(first.Recent) ||
				len(retried.Positions) != len(first.Positions) {
				t.Errorf("a retry grew the memory to %d beats and %d positions from %d and %d",
					len(retried.Recent), len(retried.Positions), len(first.Recent),
					len(first.Positions))
			}
			if diff := cmp.Diff(firstAnswer, mintAnswer(t, retried, id)); diff != "" {
				t.Errorf("a retry minted another answer (-first +retry):\n%s", diff)
			}
		})
	}
}

// mintAnswer returns the claim and head attestations a witness mints from the memory c holds, at
// the time c adopted its newest head, encoded.
func mintAnswer(t *testing.T, c *Checkpoint, id identity.Identity) string {
	t.Helper()
	claim, err := ClaimAttestation(c.LastHead, c.ObservedAt, id)
	if err != nil {
		t.Fatalf("ClaimAttestation() error = %v", err)
	}
	head, err := HeadAttestation(c.Root, c.Size, c.ObservedAt, id)
	if err != nil {
		t.Fatalf("HeadAttestation() error = %v", err)
	}
	raw, err := json.Marshal([]Countersignature{claim, head})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(raw)
}

// TestTheFirstPushIsAdoptedAndSaysSinceWhen shows the first push for an install is adopted with a
// first_root finding naming its size and root, so the statement shows from where memory holds, and
// that a first push proving from any base but nothing is told the witness holds size 0.
func TestTheFirstPushIsAdoptedAndSaysSinceWhen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	honest := testLeaves(t, 4)
	tests := []struct {
		Prev     *Checkpoint
		FromSize int64
		Want     error
	}{{ // Test 0: No memory at all.
		Prev: nil, FromSize: 0, Want: nil,
	}, { // Test 1: An empty memory that names the install and holds no tree.
		Prev: held(nil, testInstall), FromSize: 0, Want: nil,
	}, { // Test 2: A first push proving from a size this witness never held.
		Prev: nil, FromSize: 2, Want: ErrProofBase,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := testPush(t, honest, 1, 4, test.FromSize)
			next, findings, err := CheckPush(test.Prev, testInstall, p, now)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Fatalf("CheckPush() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				if !strings.Contains(err.Error(), "size 0") || next != nil || findings != nil {
					t.Errorf("CheckPush() = %v, %v, %q; want the held size 0 named and nothing "+
						"else", next, findings, err)
				}
				return
			}
			want := &Checkpoint{Server: testInstall, InstallID: testInstall, Size: 4,
				Root: testRoot(honest, 4), LastBeat: 1, LastSeq: 4, LastHead: testHead(honest, 4),
				Recent:     map[int64]Observed{1: {Seq: 4, Head: testHead(honest, 4)}},
				Positions:  map[int64]string{4: testHead(honest, 4)},
				ObservedAt: now}
			if diff := cmp.Diff(want, next); diff != "" {
				t.Errorf("checkpoint mismatch (-want +got):\n%s", diff)
			}
			if len(findings) != 1 || findings[0].Kind != "first_root" ||
				findings[0].Key != "first_root at tree size 4" {
				t.Fatalf("findings = %+v, want one first_root at tree size 4", findings)
			}
			for _, want := range []string{"size 4", testRoot(honest, 4), testInstall} {
				if !strings.Contains(findings[0].Detail, want) {
					t.Errorf("first_root detail %q does not name %s", findings[0].Detail, want)
				}
			}
		})
	}
}

// TestAPushFromAnotherBaseIsToldTheHeldSize shows a push proving from any size but the held one is
// answered with ErrProofBase naming the held size and nothing else, and that the same beat proved
// again from the held size is adopted. That exchange is what lets an install resume after the
// witness's memory was restored to an earlier size.
func TestAPushFromAnotherBaseIsToldTheHeldSize(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	honest := testLeaves(t, 9)
	prev := witnessedAt(t, honest, now, 2, 5)
	tests := []struct {
		FromSize int64
	}{{ // Test 0: From nothing.
		FromSize: 0,
	}, { // Test 1: From an earlier witnessed size.
		FromSize: 2,
	}, { // Test 2: From one short of the held size.
		FromSize: 4,
	}, { // Test 3: From past the held size, as after a restored memory.
		FromSize: 7,
	}, { // Test 4: From a negative size.
		FromSize: -1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := testPush(t, honest, 3, 9, max(test.FromSize, 0))
			p.FromSize = test.FromSize
			next, findings, err := CheckPush(prev, testInstall, p, now)
			if !errors.Is(err, ErrProofBase) || next != nil || findings != nil {
				t.Fatalf("CheckPush() = %v, %v, %v; want %v alone", next, findings, err,
					ErrProofBase)
			}
			if !strings.Contains(err.Error(), "holds the tree at size 5") {
				t.Errorf("CheckPush() error = %q, want the held size 5 named", err)
			}
			again := testPush(t, honest, 3, 9, 5)
			adopted, findings, err := CheckPush(prev, testInstall, again, now)
			if err != nil || len(findings) != 0 || !holdsPush(adopted, &again) {
				t.Errorf("the push proved again from the held size = %v, %v; want it adopted",
					findings, err)
			}
		})
	}
}

// TestAPushOutOfFormIsRefusedAndAdoptsNothing shows every push the witness cannot check is an
// ErrMalformed error with no memory and no finding, the memory it was held against unchanged,
// including a proof one hash past the bound, while a proof at the bound is checked and refused on
// its merits.
func TestAPushOutOfFormIsRefusedAndAdoptsNothing(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	id := testIdentity(t)
	honest := testLeaves(t, 8)
	prev := witnessedAt(t, honest, now, 2, 5)
	before := signedBytes(t, prev, id)
	filler := func(n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, merkle.Sum256Hex([]byte{byte(i)}))
		}
		return out
	}
	tests := []struct {
		Mutate    func(p *Push)
		Want      error
		WantKinds []string
	}{{ // Test 0: A proof one hash past the bound.
		Mutate: func(p *Push) { p.Proof = filler(maxProofHashes + 1) }, Want: ErrMalformed,
	}, { // Test 1: A proof at the bound is checked, and this one does not verify.
		Mutate:    func(p *Push) { p.Proof = filler(maxProofHashes) },
		WantKinds: []string{"rewritten_history"},
	}, { // Test 2: Beat number zero.
		Mutate: func(p *Push) { p.BeatNumber = 0 }, Want: ErrMalformed,
	}, { // Test 3: A beat number past the bound.
		Mutate: func(p *Push) { p.BeatNumber = maxBeatJump + 1 }, Want: ErrMalformed,
	}, { // Test 4: Chain position zero.
		Mutate: func(p *Push) { p.Seq = 0 }, Want: ErrMalformed,
	}, { // Test 5: A chain position past the bound.
		Mutate: func(p *Push) { p.Seq = maxBeatJump + 1 }, Want: ErrMalformed,
	}, { // Test 6: Tree size zero.
		Mutate: func(p *Push) { p.Size = 0 }, Want: ErrMalformed,
	}, { // Test 7: A tree size past the bound.
		Mutate: func(p *Push) { p.Size = maxBeatJump + 1 }, Want: ErrMalformed,
	}, { // Test 8: No head.
		Mutate: func(p *Push) { p.Head = "" }, Want: ErrMalformed,
	}, { // Test 9: The head in uppercase, a second spelling of one link.
		Mutate: func(p *Push) { p.Head = strings.ToUpper(p.Head) }, Want: ErrMalformed,
	}, { // Test 10: A head that is not a digest.
		Mutate: func(p *Push) { p.Head = "not a link" }, Want: ErrMalformed,
	}, { // Test 11: No root.
		Mutate: func(p *Push) { p.Root = "" }, Want: ErrMalformed,
	}, { // Test 12: The root in uppercase.
		Mutate: func(p *Push) { p.Root = strings.ToUpper(p.Root) }, Want: ErrMalformed,
	}, { // Test 13: A root one byte short.
		Mutate: func(p *Push) { p.Root = p.Root[:62] }, Want: ErrMalformed,
	}, { // Test 14: A proof hash that is not hex.
		Mutate: func(p *Push) { p.Proof[0] = strings.Repeat("z", 64) }, Want: ErrMalformed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := testPush(t, honest, 3, 8, 5)
			test.Mutate(&p)
			next, findings, err := CheckPush(prev, testInstall, p, now)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Fatalf("CheckPush() = %v, %v, %v; want error %v", next, findings, err, test.Want)
			}
			if string(signedBytes(t, prev, id)) != string(before) {
				t.Errorf("CheckPush() edited the previous checkpoint")
			}
			if test.Want != nil {
				if next != nil || findings != nil {
					t.Errorf("CheckPush() = %v, %v with error %v; want the error alone", next,
						findings, err)
				}
				if !strings.Contains(err.Error(), "nothing it claims was adopted") {
					t.Errorf("CheckPush() error = %q, want it to say nothing was adopted", err)
				}
				return
			}
			if diff := cmp.Diff(test.WantKinds, kindsOf(findings)); diff != "" {
				t.Errorf("finding kinds mismatch (-want +got):\n%s", diff)
			}
			if string(signedBytes(t, next, id)) != string(before) {
				t.Errorf("a push refused on its merits changed the memory the witness holds")
			}
		})
	}

	// Out of form on a first push, the answer is the same error and no memory.
	p := testPush(t, honest, 1, 4, 0)
	p.Root = ""
	next, findings, err := CheckPush(nil, testInstall, p, now)
	if !errors.Is(err, ErrMalformed) || next != nil || findings != nil {
		t.Fatalf("CheckPush() of a first push out of form = %v, %v, %v; want %v alone", next,
			findings, err, ErrMalformed)
	}
}

// TestCheckRulesStillHoldForAPush shows every rule Check holds a pulled beat to applies to a push
// whose tree proof is valid: a reissued beat, a regressed position, a contradicted position, and an
// older beat are each refused with Check's own finding. A gap in beat numbers over a valid proof is
// named and adopted, since the proof shows nothing witnessed changed, and the same beat pushed
// again over a larger tree is refused. A refusal a day after the held head raises only itself,
// since the stalled feed Check would add describes a pulled server, not a push.
func TestCheckRulesStillHoldForAPush(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	id := testIdentity(t)
	honest := testLeaves(t, 9)
	prev := witnessedAt(t, honest, now, 3, 5)
	before := signedBytes(t, prev, id)
	tests := []struct {
		Mutate      func(p *Push)
		After       time.Duration
		WantKinds   []string
		WantAdopted bool
	}{{ // Test 0: Beat 2 reissued at another position and link.
		Mutate: func(p *Push) { p.BeatNumber = 2 }, WantKinds: []string{"rewritten_beat"},
	}, { // Test 1: A new beat at a position behind the witnessed one.
		Mutate: func(p *Push) {
			p.Seq, p.Head = 4, testHead(honest, 4)
		},
		WantKinds: []string{"seq_regression"},
	}, { // Test 2: A new beat contradicting the link witnessed at position 3.
		Mutate: func(p *Push) {
			p.Seq, p.Head = 3, testHead(honest, 6)
		},
		WantKinds: []string{"rewritten_history", "seq_regression"},
	}, { // Test 3: Beat 1 again, exactly as witnessed, over the newer tree.
		Mutate: func(p *Push) {
			p.BeatNumber, p.Seq, p.Head = 1, 3, testHead(honest, 3)
		},
		WantKinds: []string{"head_regression"},
	}, { // Test 4: Beats 3 and 4 never pushed, as when an install's queue dropped them.
		Mutate: func(p *Push) { p.BeatNumber = 5 }, WantKinds: []string{"missing_beat"},
		WantAdopted: true,
	}, { // Test 5: Beat 2 pushed again over a larger tree.
		Mutate: func(p *Push) {
			p.BeatNumber, p.Seq, p.Head = 2, 5, testHead(honest, 5)
		},
		WantKinds: []string{"duplicate_beat"},
	}, { // Test 6: Beat 2 reissued a day after the held head was adopted.
		Mutate: func(p *Push) { p.BeatNumber = 2 }, After: StaleAfter + time.Hour,
		WantKinds: []string{"rewritten_beat"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := testPush(t, honest, 3, 9, 5)
			test.Mutate(&p)
			next, findings, err := CheckPush(prev, testInstall, p, now.Add(test.After))
			if err != nil {
				t.Fatalf("CheckPush() error = %v", err)
			}
			if diff := cmp.Diff(test.WantKinds, kindsOf(findings)); diff != "" {
				t.Errorf("finding kinds mismatch (-want +got):\n%s", diff)
			}
			if got := holdsPush(next, &p); got != test.WantAdopted {
				t.Errorf("adopted = %v, want %v", got, test.WantAdopted)
			}
			if !test.WantAdopted && string(signedBytes(t, next, id)) != string(before) {
				t.Errorf("a refused push changed the memory the witness holds")
			}
		})
	}
}

// TestCheckPushHoldsEachMemoryToItsOwnInstall shows a push is refused with an error, and no
// memory, when it names no install, names another install than the one it is checked as, or is
// held against a memory of another install or of a pulled server.
func TestCheckPushHoldsEachMemoryToItsOwnInstall(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	honest := testLeaves(t, 4)
	pulled, _, err := Check(nil, "https://st.example", []Beat{beat(1, 1, link("a"))}, now)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	other := held(nil, "in_ffffffffffffffffffffffffffffffff")
	otherServer := held(nil, testInstall)
	otherServer.Server = "in_ffffffffffffffffffffffffffffffff"
	otherServer.InstallID = ""
	tests := []struct {
		Prev      *Checkpoint
		InstallID string
		PushFor   string
		Want      error
		WantText  string
	}{{ // Test 0: No install named.
		InstallID: "", PushFor: "", Want: ErrMalformed, WantText: "none was named",
	}, { // Test 1: The push names another install.
		InstallID: testInstall, PushFor: "in_ffffffffffffffffffffffffffffffff",
		Want: ErrMalformed, WantText: "names install",
	}, { // Test 2: The memory belongs to another install.
		Prev: other, InstallID: testInstall, PushFor: testInstall,
		WantText: "remembers install",
	}, { // Test 3: The memory is a pulled server's.
		Prev: pulled, InstallID: testInstall, PushFor: testInstall,
		WantText: "https://st.example",
	}, { // Test 4: The memory names another install as its subject alone.
		Prev: otherServer, InstallID: testInstall, PushFor: testInstall,
		WantText: "in_ffffffffffffffffffffffffffffffff",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := testPush(t, honest, 1, 4, 0)
			p.InstallID = test.PushFor
			next, findings, err := CheckPush(test.Prev, test.InstallID, p, now)
			if err == nil || next != nil || findings != nil {
				t.Fatalf("CheckPush() = %v, %v, %v; want an error alone", next, findings, err)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("CheckPush() error = %v, want %v", err, test.Want)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("CheckPush() error = %q, want it to say %q", err, test.WantText)
			}
		})
	}
}

// proofLen counts the hashes merkle.ConsistencyProof returns for a prefix of m in a tree of n,
// following the same recursion without hashing anything, so the length at sizes far too large to
// build can be measured.
func proofLen(m, n int64, complete bool) int {
	if m == n {
		if complete {
			return 0
		}
		return 1
	}
	k := int64(1)
	for k<<1 < n {
		k <<= 1
	}
	if m <= k {
		return proofLen(m, k, complete) + 1
	}
	return proofLen(m-k, n-k, false) + 1
}

// TestTheProofBoundAdmitsEveryHonestProof shows the proof bound never refuses a proof an honest
// install builds. The counting recursion is first held to the merkle package's own proofs for every
// prefix of every tree up to 24 entries, then measured at the largest tree size a push may claim,
// where the longest proof is forty-one hashes.
func TestTheProofBoundAdmitsEveryHonestProof(t *testing.T) {
	t.Parallel()
	leaves := testLeaves(t, 24)
	for n := int64(1); n <= 24; n++ {
		for m := int64(1); m <= n; m++ {
			proof, err := merkle.ConsistencyProof(m, leaves[:n])
			if err != nil {
				t.Fatalf("ConsistencyProof(%d, %d) error = %v", m, n, err)
			}
			if got := proofLen(m, n, true); got != len(proof) {
				t.Fatalf("proofLen(%d, %d) = %d, merkle builds %d", m, n, got, len(proof))
			}
		}
	}
	longest := 0
	for _, n := range []int64{maxBeatJump, maxBeatJump - 1, maxBeatJump/2 + 1, 3 * maxBeatJump / 4} {
		firsts := []int64{1, 2, 3, n / 2, n/2 + 1, n - 1, n, 0x5555555555 % n, 0xAAAAAAAAAA % n}
		for i := range 41 {
			firsts = append(firsts, max(int64(1)<<i-1, 1), min(int64(1)<<i+1, n))
		}
		for _, m := range firsts {
			if m < 1 || m > n {
				continue
			}
			longest = max(longest, proofLen(m, n, true))
		}
	}
	if longest > maxProofHashes {
		t.Errorf("an honest proof at the largest tree is %d hashes, past the bound of %d", longest,
			maxProofHashes)
	}
	if longest != 41 {
		t.Errorf("the longest proof measured is %d hashes, want 41 as the bound's comment says",
			longest)
	}
}
