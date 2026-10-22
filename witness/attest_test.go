package witness

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/identity"
)

// loomsealModuleDir resolves the directory of the loomseal module this build compiles against, once
// per test binary, so the vectors read are the ones shipped with the exact version in go.mod.
var loomsealModuleDir = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/kordloom/loomseal").Output()
	if err != nil {
		return "", fmt.Errorf("go list loomseal: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errors.New("go list named no directory for the loomseal module")
	}
	return dir, nil
})

// loomsealVector is the part of a LoomSeal conformance vector these tests read.
type loomsealVector struct {
	// Attestations is the bundle's top-level head attestations.
	Attestations []Countersignature `json:"attestations"`
	// Chain is the bundle's chain head.
	Chain struct {
		// Head is the signed head the head attestations vouch for.
		Head struct {
			// Link is the head link.
			Link string `json:"link"`
			// Seq is the head position.
			Seq int64 `json:"seq"`
		} `json:"head"`
	} `json:"chain"`
	// Claims is the bundle's claims.
	Claims []struct {
		// Attestations is the claim's counter-signatures.
		Attestations []Countersignature `json:"attestations"`
		// Chain is the claim's chain position.
		Chain struct {
			// Link is the claim's link.
			Link string `json:"link"`
		} `json:"chain"`
	} `json:"claims"`
}

// readLoomsealVector reads one named conformance vector from the loomseal module and fails unless
// the module's own manifest says a conforming verifier must accept it.
func readLoomsealVector(t *testing.T, name string) loomsealVector {
	t.Helper()
	dir, err := loomsealModuleDir()
	if err != nil {
		t.Fatalf("locate the loomseal module: %v", err)
	}
	vectors := filepath.Join(dir, "testdata", "vectors")
	raw, err := os.ReadFile(filepath.Join(vectors, "manifest.json"))
	if err != nil {
		t.Fatalf("read the loomseal vector manifest: %v", err)
	}
	var manifest struct {
		// Vectors lists every vector with its verdict.
		Vectors []struct {
			// Name is the vector's name.
			Name string `json:"name"`
			// File is the vector's file name.
			File string `json:"file"`
			// MustVerify is whether a conforming verifier accepts it.
			MustVerify bool `json:"must_verify"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse the loomseal vector manifest: %v", err)
	}
	file := ""
	for _, v := range manifest.Vectors {
		if v.Name == name {
			if !v.MustVerify {
				t.Fatalf("loomseal vector %s is one a verifier must refuse", name)
			}
			file = v.File
		}
	}
	if file == "" {
		t.Fatalf("the loomseal manifest has no vector named %s", name)
	}
	raw, err = os.ReadFile(filepath.Join(vectors, file))
	if err != nil {
		t.Fatalf("read loomseal vector %s: %v", name, err)
	}
	var v loomsealVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse loomseal vector %s: %v", name, err)
	}
	return v
}

// vectorCounterparty returns the identity the loomseal vector generator signs every counterparty
// and witness attestation with: the ed25519 key from the seed 200, 199, and so on down.
func vectorCounterparty(t *testing.T) identity.Identity {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(200 - i)
	}
	dir := t.TempDir()
	stored := fmt.Sprintf(`{"install_id":"","seed":%q}`, hex.EncodeToString(seed))
	path := filepath.Join(dir, identity.WitnessFile)
	if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
		t.Fatalf("write the vector key: %v", err)
	}
	id, err := identity.LoadWitnessFile(dir)
	if err != nil {
		t.Fatalf("LoadWitnessFile() error = %v", err)
	}
	return id
}

// vectorTime is the signing time the loomseal vectors carry.
var vectorTime = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// TestHeadAttestationIsTheLoomSealVectorByteForByte mints a head attestation with this package over
// the head of the loomseal module's head-attested vector, with that vector's key and time, and
// requires the result to equal the vector's attestation member for member. ed25519 signing is
// deterministic, so an equal sig means the preimage is byte-equal to the one the format's own
// generator signed, and the manifest marks that vector as one every conforming verifier accepts.
func TestHeadAttestationIsTheLoomSealVectorByteForByte(t *testing.T) {
	t.Parallel()
	v := readLoomsealVector(t, "head-attested")
	if len(v.Attestations) != 1 {
		t.Fatalf("head-attested carries %d head attestations, want 1", len(v.Attestations))
	}
	got, err := HeadAttestation(v.Chain.Head.Link, v.Chain.Head.Seq, vectorTime,
		vectorCounterparty(t))
	if err != nil {
		t.Fatalf("HeadAttestation() error = %v", err)
	}
	if diff := cmp.Diff(v.Attestations[0], got); diff != "" {
		t.Errorf("HeadAttestation() differs from the loomseal vector (-want +got):\n%s", diff)
	}
	err = VerifyHeadAttestation(v.Chain.Head.Link, v.Chain.Head.Seq, v.Attestations[0])
	if err != nil {
		t.Errorf("VerifyHeadAttestation() of the loomseal vector error = %v", err)
	}
}

// TestClaimAttestationIsTheLoomSealVectorByteForByte mints claim attestations with this package's
// preimage over the claims of two loomseal vectors, one without a time and one with, and requires
// each to equal the vector's attestation. The vectors sign as a counterparty and an approver, so
// the role is the vector's. VerifyClaimAttestation then refuses them, because neither is a witness.
func TestClaimAttestationIsTheLoomSealVectorByteForByte(t *testing.T) {
	t.Parallel()
	id := vectorCounterparty(t)
	tests := []struct {
		Vector string
	}{{ // Test 0: A counterparty attestation without a time.
		Vector: "attested-claim",
	}, { // Test 1: An approver attestation with a time.
		Vector: "attestation-with-time",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			v := readLoomsealVector(t, test.Vector)
			if len(v.Claims) == 0 || len(v.Claims[0].Attestations) != 1 {
				t.Fatalf("%s does not carry one claim attestation", test.Vector)
			}
			want := v.Claims[0].Attestations[0]
			link := v.Claims[0].Chain.Link
			got, err := mint(claimPreimage(link, want.Role, want.At), want.Role, want.At,
				id.Private())
			if err != nil {
				t.Fatalf("mint() error = %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("claim attestation differs from %s (-want +got):\n%s", test.Vector, diff)
			}
			if err := VerifyClaimAttestation(link, want); !errors.Is(err, ErrMalformed) {
				t.Errorf("VerifyClaimAttestation() of a %s attestation error = %v, want %v",
					want.Role, err, ErrMalformed)
			}
		})
	}
}

// TestClaimAttestationIsTheLoomSealPreimage pins the exact bytes a witness claim attestation signs,
// the RFC 8785 form of FORMAT.md's attestation/1 object, and that the exported function signs them.
func TestClaimAttestationIsTheLoomSealPreimage(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	link := testHash('a')
	got, err := ClaimAttestation(link, vectorTime, id)
	if err != nil {
		t.Fatalf("ClaimAttestation() error = %v", err)
	}
	preimage := `{"at":"2026-08-30T12:00:00Z","link":"` + link +
		`","loomseal":"attestation/1","role":"witness"}`
	sig, err := decodeBase64(got.Sig)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ed25519.Verify(id.Public(), []byte(preimage), sig) {
		t.Errorf("ClaimAttestation() did not sign %s", preimage)
	}
	want := Countersignature{KeyID: id.KeyID(), PublicKey: id.PublicKeyBase64(), Alg: "ed25519",
		Role: "witness", At: "2026-08-30T12:00:00Z", Sig: got.Sig}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ClaimAttestation() mismatch (-want +got):\n%s", diff)
	}
}

// TestClaimAttestationBindsEveryField mints a witness claim attestation and edits each member, and
// the claim it is checked against, in turn: the honest one verifies and every edit is refused.
func TestClaimAttestationBindsEveryField(t *testing.T) {
	t.Parallel()
	id, other := testIdentity(t), testIdentity(t)
	link := testHash('a')
	signed, err := ClaimAttestation(link, vectorTime, id)
	if err != nil {
		t.Fatalf("ClaimAttestation() error = %v", err)
	}
	byOther, err := ClaimAttestation(link, vectorTime, other)
	if err != nil {
		t.Fatalf("ClaimAttestation() error = %v", err)
	}
	elsewhere := mustClaim(t, testHash('c'), id)
	tests := []struct {
		Field  string
		Link   string
		Mutate func(c *Countersignature)
		Want   error
	}{{ // Test 0: The honest attestation verifies.
		Field: "", Link: link, Mutate: func(*Countersignature) {}, Want: nil,
	}, { // Test 1: Moved to another claim.
		Field: "link", Link: testHash('b'), Mutate: func(*Countersignature) {}, Want: ErrSignature,
	}, { // Test 2: Another key with its own key id, keeping this signature.
		Field: "public_key", Link: link, Mutate: func(c *Countersignature) {
			c.PublicKey, c.KeyID = byOther.PublicKey, byOther.KeyID
		}, Want: ErrSignature,
	}, { // Test 3: Another key under this key's id.
		Field: "public_key", Link: link,
		Mutate: func(c *Countersignature) { c.PublicKey = byOther.PublicKey }, Want: ErrMalformed,
	}, { // Test 4: Another key id.
		Field: "key_id", Link: link, Mutate: func(c *Countersignature) { c.KeyID = byOther.KeyID },
		Want: ErrMalformed,
	}, { // Test 5: Another algorithm.
		Field: "alg", Link: link, Mutate: func(c *Countersignature) { c.Alg = "ed448" },
		Want: ErrMalformed,
	}, { // Test 6: Another role.
		Field: "role", Link: link, Mutate: func(c *Countersignature) { c.Role = "auditor" },
		Want: ErrMalformed,
	}, { // Test 7: Another time.
		Field: "at", Link: link,
		Mutate: func(c *Countersignature) { c.At = "2026-08-30T12:00:01Z" }, Want: ErrSignature,
	}, { // Test 8: The time removed.
		Field: "at", Link: link, Mutate: func(c *Countersignature) { c.At = "" },
		Want: ErrMalformed,
	}, { // Test 9: Another signature by the same key.
		Field: "sig", Link: link, Mutate: func(c *Countersignature) { c.Sig = elsewhere.Sig },
		Want: ErrSignature,
	}, { // Test 10: The signature with a line break.
		Field: "sig", Link: link, Mutate: func(c *Countersignature) { c.Sig = "\n" + c.Sig },
		Want: ErrMalformed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			c := signed
			test.Mutate(&c)
			err := VerifyClaimAttestation(test.Link, c)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyClaimAttestation() after editing %q error = %v, want %v",
					test.Field, err, test.Want)
			}
		})
	}
}

// mustClaim mints a witness claim attestation over link or fails the test.
func mustClaim(t *testing.T, link string, id identity.Identity) Countersignature {
	t.Helper()
	c, err := ClaimAttestation(link, vectorTime, id)
	if err != nil {
		t.Fatalf("ClaimAttestation() error = %v", err)
	}
	return c
}

// TestHeadAttestationBindsEveryField mints a witness head attestation and edits each member, the
// root, and the size in turn: the honest one verifies and every edit is refused.
func TestHeadAttestationBindsEveryField(t *testing.T) {
	t.Parallel()
	id, other := testIdentity(t), testIdentity(t)
	root := testHash('a')
	signed, err := HeadAttestation(root, 90213, vectorTime, id)
	if err != nil {
		t.Fatalf("HeadAttestation() error = %v", err)
	}
	tests := []struct {
		Field  string
		Root   string
		Size   int64
		Mutate func(c *Countersignature)
		Want   error
	}{{ // Test 0: The honest attestation verifies.
		Field: "", Root: root, Size: 90213, Mutate: func(*Countersignature) {}, Want: nil,
	}, { // Test 1: Another root.
		Field: "root", Root: testHash('b'), Size: 90213, Mutate: func(*Countersignature) {},
		Want: ErrSignature,
	}, { // Test 2: Another size.
		Field: "size", Root: root, Size: 90214, Mutate: func(*Countersignature) {},
		Want: ErrSignature,
	}, { // Test 3: Another key with its own key id.
		Field: "public_key", Root: root, Size: 90213, Mutate: func(c *Countersignature) {
			c.PublicKey, c.KeyID = other.PublicKeyBase64(), other.KeyID()
		}, Want: ErrSignature,
	}, { // Test 4: Another role.
		Field: "role", Root: root, Size: 90213,
		Mutate: func(c *Countersignature) { c.Role = "custodian" }, Want: ErrMalformed,
	}, { // Test 5: Another time.
		Field: "at", Root: root, Size: 90213,
		Mutate: func(c *Countersignature) { c.At = "2026-08-30T12:00:00.5Z" }, Want: ErrSignature,
	}, { // Test 6: Another algorithm.
		Field: "alg", Root: root, Size: 90213, Mutate: func(c *Countersignature) { c.Alg = "" },
		Want: ErrMalformed,
	}, { // Test 7: The signature truncated.
		Field: "sig", Root: root, Size: 90213, Mutate: func(c *Countersignature) { c.Sig = "AAAA" },
		Want: ErrMalformed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			c := signed
			test.Mutate(&c)
			err := VerifyHeadAttestation(test.Root, test.Size, c)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyHeadAttestation() after editing %q error = %v, want %v",
					test.Field, err, test.Want)
			}
		})
	}
}

// TestAttestationKindsDoNotCrossVerify shows the two domain tags keep a head attestation from
// passing as a claim attestation over the same hash and the reverse, and that the producer's push
// tag signs nothing a witness attestation verifies.
func TestAttestationKindsDoNotCrossVerify(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	hash := testHash('a')
	claim := mustClaim(t, hash, id)
	head, err := HeadAttestation(hash, 1, vectorTime, id)
	if err != nil {
		t.Fatalf("HeadAttestation() error = %v", err)
	}
	later := map[string]any{"loomseal": "attestation/2", "link": hash, "role": RoleWitness,
		"at": claim.At}
	other, err := mint(later, RoleWitness, claim.At, id.Private())
	if err != nil {
		t.Fatalf("mint() error = %v", err)
	}
	tests := []struct {
		Call func() error
	}{{ // Test 0: A head attestation checked as a claim attestation.
		Call: func() error { return VerifyClaimAttestation(hash, head) },
	}, { // Test 1: A claim attestation checked as a head attestation.
		Call: func() error { return VerifyHeadAttestation(hash, 1, claim) },
	}, { // Test 2: A claim attestation under a later domain tag.
		Call: func() error { return VerifyClaimAttestation(hash, other) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := test.Call(); !errors.Is(err, ErrSignature) {
				t.Errorf("error = %v, want %v", err, ErrSignature)
			}
		})
	}
}

// TestAttestationsRefuseWhatTheyCannotSign shows minting refuses a link or root that is not a
// lowercase sha256 hex digest, a size below one, a time the LoomSeal form cannot write, and an
// identity with no key.
func TestAttestationsRefuseWhatTheyCannotSign(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		Head bool
		Hash string
		Size int64
		At   time.Time
		ID   identity.Identity
	}{{ // Test 0: A claim link that is not hex.
		Hash: "not a link", At: vectorTime, ID: id,
	}, { // Test 1: A claim link in upper case.
		Hash: strings.ToUpper(testHash('a')), At: vectorTime, ID: id,
	}, { // Test 2: A claim with no time.
		Hash: testHash('a'), At: time.Time{}, ID: id,
	}, { // Test 3: A claim with no key.
		Hash: testHash('a'), At: vectorTime, ID: identity.Identity{},
	}, { // Test 4: A head root one digit short.
		Head: true, Hash: testHash('a')[1:], Size: 1, At: vectorTime, ID: id,
	}, { // Test 5: A head of size zero.
		Head: true, Hash: testHash('a'), Size: 0, At: vectorTime, ID: id,
	}, { // Test 6: A head time past year 9999.
		Head: true, Hash: testHash('a'), Size: 1, At: far, ID: id,
	}, { // Test 7: A head size past what the canonical form carries.
		Head: true, Hash: testHash('a'), Size: 1 << 60, At: vectorTime, ID: id,
	}, { // Test 8: A head with no key.
		Head: true, Hash: testHash('a'), Size: 1, At: vectorTime, ID: identity.Identity{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var err error
			if test.Head {
				_, err = HeadAttestation(test.Hash, test.Size, test.At, test.ID)
			} else {
				_, err = ClaimAttestation(test.Hash, test.At, test.ID)
			}
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("error = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

// TestSealTimeIsTheOneLoomSealForm pins the signing time to the form FORMAT.md's canonical time
// rule states, both as minted and as accepted.
func TestSealTimeIsTheOneLoomSealForm(t *testing.T) {
	t.Parallel()
	chicago := time.FixedZone("CDT", -5*60*60)
	formats := []struct {
		WantText string
		In       time.Time
	}{{ // Test 0: Whole seconds carry no fraction.
		In: vectorTime, WantText: "2026-08-30T12:00:00Z",
	}, { // Test 1: Another zone is written in UTC.
		In: time.Date(2026, 8, 30, 7, 0, 0, 0, chicago), WantText: "2026-08-30T12:00:00Z",
	}, { // Test 2: Trailing fractional zeros are trimmed.
		In:       time.Date(2026, 8, 30, 12, 0, 0, 500_000_000, time.UTC),
		WantText: "2026-08-30T12:00:00.5Z",
	}, { // Test 3: Nanoseconds are kept.
		In:       time.Date(2026, 8, 30, 12, 0, 0, 123_456_789, time.UTC),
		WantText: "2026-08-30T12:00:00.123456789Z",
	}}
	for testNum, test := range formats {
		t.Run(fmt.Sprintf("format %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := formatSealTime(test.In)
			if err != nil {
				t.Fatalf("formatSealTime() error = %v", err)
			}
			if diff := cmp.Diff(test.WantText, got); diff != "" {
				t.Errorf("formatSealTime() mismatch (-want +got):\n%s", diff)
			}
			if !validSealTime(got) {
				t.Errorf("validSealTime(%q) = false for a minted time", got)
			}
		})
	}
	parses := []struct {
		In        string
		WantValid bool
	}{{ // Test 0: The plain form.
		In: "2026-08-30T12:00:00Z", WantValid: true,
	}, { // Test 1: A fraction with a trailing zero is still the form.
		In: "2026-08-30T12:00:00.10Z", WantValid: true,
	}, { // Test 2: An offset is not.
		In: "2026-08-30T12:00:00+00:00", WantValid: false,
	}, { // Test 3: A lower case separator is not.
		In: "2026-08-30t12:00:00Z", WantValid: false,
	}, { // Test 4: A one digit hour is not.
		In: "2026-08-30T1:00:00Z", WantValid: false,
	}, { // Test 5: A leap second is not.
		In: "2026-12-31T23:59:60Z", WantValid: false,
	}, { // Test 6: A day past the end of February is not.
		In: "2026-02-29T00:00:00Z", WantValid: false,
	}, { // Test 7: Year zero is not.
		In: "0000-01-01T00:00:00Z", WantValid: false,
	}, { // Test 8: Empty is not.
		In: "", WantValid: false,
	}, { // Test 9: A bare dot is not.
		In: "2026-08-30T12:00:00.Z", WantValid: false,
	}}
	for testNum, test := range parses {
		t.Run(fmt.Sprintf("parse %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := validSealTime(test.In); got != test.WantValid {
				t.Errorf("validSealTime(%q) = %v, want %v", test.In, got, test.WantValid)
			}
		})
	}
}
