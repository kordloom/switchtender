package audit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestReadTimestampProofNamesTheSigner proves the offline token check hands back the certificate
// the token's signature verified under, and every certificate it carries, so a verifier can say
// whose token it read. A token from an authority made inside this test verifies, because a token is
// checked against the certificate it carries, and that is exactly why its signer has to be named.
func TestReadTimestampProofNamesTheSigner(t *testing.T) {
	t.Parallel()
	link := hex.EncodeToString([]byte("any head a key holder wants timestamped"))
	raw, err := hex.DecodeString(link)
	if err != nil {
		t.Fatalf("decode link: %v", err)
	}
	sum := sha256.Sum256(raw)
	proof := base64.StdEncoding.EncodeToString(tokenOver(t, sum[:]))
	auth, err := testAuthority()
	if err != nil {
		t.Fatalf("build the test authority: %v", err)
	}

	att, err := ReadTimestampProof(link, proof)
	if err != nil {
		t.Fatalf("ReadTimestampProof() error = %v", err)
	}
	if !att.Signer.Equal(auth.Cert) {
		t.Errorf("signer = %q, want the authority made in this test", att.Signer.Subject)
	}
	if len(att.Carried) != 1 || !att.Carried[0].Equal(auth.Cert) {
		t.Errorf("carried %d certificates, want the authority's one", len(att.Carried))
	}
	if !att.GenTime.Equal(genTime()) {
		t.Errorf("GenTime = %s, want %s", att.GenTime, genTime())
	}
	when, err := VerifyTimestampProofTime(link, proof)
	if err != nil || !when.Equal(genTime()) {
		t.Errorf("VerifyTimestampProofTime() = %s, %v, want %s and no error", when, err, genTime())
	}
}

// TestSelfIssuedAuthorityCountsOnlyWhenPinned is the self-issued authority case. A token signed by
// a certificate made on the spot verifies, so a producer who rewrote their chain could sign a
// backdated token over the new head, and counting it as an anchor would vouch for the rewrite.
// With no pin the signer is named, marked unpinned, and left out of the count while the verdict
// stays what it was. Pinned by its own fingerprint, in any form a reader copies, it counts. Under a
// pin it does not match, the anchors fail and the problem names the signer.
func TestSelfIssuedAuthorityCountsOnlyWhenPinned(t *testing.T) {
	// No t.Parallel: this sets an environment variable, so it must not run beside another test.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	auth, err := testAuthority()
	if err != nil {
		t.Fatalf("build the test authority: %v", err)
	}
	sum := sha256.Sum256(auth.Cert.Raw)
	own := "sha256:" + hex.EncodeToString(sum[:])
	signer := func(pinnedBy string) []TimestampSigner {
		return []TimestampSigner{{
			Seq: 3, Subject: "CN=switchtender test timestamp authority", Fingerprint: own,
			PinnedBy: pinnedBy,
		}}
	}
	other := "sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		WantSigners     []TimestampSigner
		Pins            []string
		Name            string
		Published       bool
		WantOK          bool
		WantCounted     int
		WantPinProblems int
	}{{ // Test 0: No pin, the token alone is named, unpinned, and not counted.
		Name: "no pin", WantSigners: signer(""), WantOK: true, WantCounted: 0,
	}, { // Test 1: No pin, beside an anchor that carries no token only that anchor is counted.
		Name: "no pin beside a published anchor", Published: true, WantSigners: signer(""),
		WantOK: true, WantCounted: 1,
	}, { // Test 2: Its own fingerprint pinned, the token counts.
		Name: "own fingerprint pinned", Pins: []string{own}, WantSigners: signer(PinnedBySigner),
		WantOK: true, WantCounted: 1,
	}, { // Test 3: The same pin as openssl prints it, upper case and colon separated.
		Name: "own fingerprint pinned in the openssl form", Pins: []string{opensslForm(own)},
		WantSigners: signer(PinnedBySigner), WantOK: true, WantCounted: 1,
	}, { // Test 4: A different pin fails the anchors and names the signer.
		Name: "different pin", Pins: []string{other}, WantSigners: signer(""),
		WantOK: false, WantCounted: 0, WantPinProblems: 1,
	}, { // Test 5: A different pin beside a published anchor still fails.
		Name: "different pin beside a published anchor", Pins: []string{other}, Published: true,
		WantSigners: signer(""), WantOK: false, WantCounted: 1, WantPinProblems: 1,
	}, { // Test 6: Either of two pins is enough, as with any repeated pin.
		Name: "one of two pins", Pins: []string{other, own}, WantSigners: signer(PinnedBySigner),
		WantOK: true, WantCounted: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			signed, keyID := anchoredBundle(t, test.Published, func(digest []byte) []byte {
				return tokenOver(t, digest)
			})
			rep, err := VerifyBundle(signed, keyID, WithAuthorityPins(test.Pins...))
			if err != nil {
				t.Fatalf("%s: VerifyBundle() error = %v", test.Name, err)
			}
			if rep.OK() != test.WantOK || rep.AnchorsOK != test.WantOK {
				t.Errorf("%s: OK = %t, AnchorsOK = %t, want %t, problems %q %q", test.Name,
					rep.OK(), rep.AnchorsOK, test.WantOK, rep.TimestampProblems, rep.PinProblems)
			}
			if rep.TimestampsVerified != 1 {
				t.Errorf("%s: TimestampsVerified = %d, want 1", test.Name, rep.TimestampsVerified)
			}
			if rep.AnchorsCounted != test.WantCounted {
				t.Errorf("%s: AnchorsCounted = %d, want %d", test.Name, rep.AnchorsCounted,
					test.WantCounted)
			}
			if rep.AuthorityPins != len(test.Pins) {
				t.Errorf("%s: AuthorityPins = %d, want %d", test.Name, rep.AuthorityPins,
					len(test.Pins))
			}
			if diff := cmp.Diff(test.WantSigners, rep.TimestampSigners); diff != "" {
				t.Errorf("%s: TimestampSigners mismatch (-want +got):\n%s", test.Name, diff)
			}
			if len(rep.PinProblems) != test.WantPinProblems {
				t.Fatalf("%s: PinProblems = %q, want %d", test.Name, rep.PinProblems,
					test.WantPinProblems)
			}
			for _, p := range rep.PinProblems {
				if !strings.Contains(p, own) || !strings.Contains(p, "test timestamp authority") {
					t.Errorf("%s: the pin problem does not name the signer: %q", test.Name, p)
				}
			}
		})
	}
}

// TestCarriedIssuerPinCounts proves the second arm of the pin. An authority reissues its signing
// certificate on its own schedule, so a reader pins its long-lived certificate instead, and a token
// counts when it carries that certificate and the certificate directly signed the signer's. One
// step only: a carried certificate that signed nothing here, or one not marked to sign
// certificates, does not stand in for the signer, so pinning it fails the anchors.
func TestCarriedIssuerPinCounts(t *testing.T) {
	// No t.Parallel: this sets an environment variable, so it must not run beside another test.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	root := mintAuthority(t, "test root authority", nil, true)
	leaf := mintAuthority(t, "test signing authority", root, false)
	stranger := mintAuthority(t, "test stranger authority", nil, true)
	plain := mintAuthority(t, "test plain issuer", nil, false)
	plainLeaf := mintAuthority(t, "test signing authority under a plain issuer", plain, false)
	tests := []struct {
		Token        func(digest []byte) []byte
		Name         string
		Pin          string
		WantIssuer   string
		WantPinnedBy string
		WantOK       bool
		WantCounted  int
	}{{ // Test 0: The issuer pinned, the token counts through it.
		Name: "issuer pinned", Pin: certFingerprint(root.Cert),
		Token:      func(d []byte) []byte { return tokenBy(t, d, leaf, leaf.Cert, root.Cert) },
		WantIssuer: certFingerprint(root.Cert), WantPinnedBy: PinnedByIssuer, WantOK: true,
		WantCounted: 1,
	}, { // Test 1: The signer pinned, the issuer is still named and the signer is what matched.
		Name: "signer pinned", Pin: certFingerprint(leaf.Cert),
		Token:      func(d []byte) []byte { return tokenBy(t, d, leaf, leaf.Cert, root.Cert) },
		WantIssuer: certFingerprint(root.Cert), WantPinnedBy: PinnedBySigner, WantOK: true,
		WantCounted: 1,
	}, { // Test 2: No pin, the issuer is named for the reader to pin, and nothing counts.
		Name:       "no pin",
		Token:      func(d []byte) []byte { return tokenBy(t, d, leaf, leaf.Cert, root.Cert) },
		WantIssuer: certFingerprint(root.Cert), WantOK: true,
	}, { // Test 3: A carried certificate that signed nothing here does not count when pinned.
		Name: "stranger pinned", Pin: certFingerprint(stranger.Cert),
		Token: func(d []byte) []byte {
			return tokenBy(t, d, leaf, stranger.Cert, leaf.Cert, root.Cert)
		},
		WantIssuer: certFingerprint(root.Cert), WantOK: false,
	}, { // Test 4: The issuer pinned, but the token does not carry it, so the step cannot be checked.
		Name: "issuer pinned and not carried", Pin: certFingerprint(root.Cert),
		Token:  func(d []byte) []byte { return tokenBy(t, d, leaf, leaf.Cert) },
		WantOK: false,
	}, { // Test 5: An issuer not marked to sign certificates does not count when pinned.
		Name: "plain issuer pinned", Pin: certFingerprint(plain.Cert),
		Token: func(d []byte) []byte {
			return tokenBy(t, d, plainLeaf, plainLeaf.Cert, plain.Cert)
		},
		WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			signed, keyID := anchoredBundle(t, false, test.Token)
			var opts []VerifyOption
			if test.Pin != "" {
				opts = append(opts, WithAuthorityPins(test.Pin))
			}
			rep, err := VerifyBundle(signed, keyID, opts...)
			if err != nil {
				t.Fatalf("%s: VerifyBundle() error = %v", test.Name, err)
			}
			if rep.OK() != test.WantOK {
				t.Errorf("%s: OK = %t, want %t, problems %q %q", test.Name, rep.OK(), test.WantOK,
					rep.TimestampProblems, rep.PinProblems)
			}
			if rep.AnchorsCounted != test.WantCounted {
				t.Errorf("%s: AnchorsCounted = %d, want %d", test.Name, rep.AnchorsCounted,
					test.WantCounted)
			}
			if len(rep.TimestampSigners) != 1 {
				t.Fatalf("%s: %d signers named, want 1", test.Name, len(rep.TimestampSigners))
			}
			got := rep.TimestampSigners[0]
			if got.IssuerFingerprint != test.WantIssuer {
				t.Errorf("%s: issuer = %q, want %q", test.Name, got.IssuerFingerprint, test.WantIssuer)
			}
			if got.PinnedBy != test.WantPinnedBy {
				t.Errorf("%s: PinnedBy = %q, want %q", test.Name, got.PinnedBy, test.WantPinnedBy)
			}
		})
	}
}

// TestAuthorityPinFingerprint pins the forms a timestamp authority pin is read in. A reader copies
// it from openssl, from a web page, or from this tool's own output, and each form names the same
// certificate. Anything else is refused, because a pin that read as nothing would leave every
// token unpinned and the verdict unchanged.
func TestAuthorityPinFingerprint(t *testing.T) {
	t.Parallel()
	hexForm := strings.Repeat("0a", 32)
	want := "sha256:" + hexForm
	tests := []struct {
		WantResult string
		Want       error
		In         string
	}{{ // Test 0: This tool's own form.
		In: want, WantResult: want,
	}, { // Test 1: Bare hex.
		In: hexForm, WantResult: want,
	}, { // Test 2: Upper case with the prefix.
		In: "SHA256:" + strings.ToUpper(hexForm), WantResult: want,
	}, { // Test 3: The colon pairs openssl prints.
		In: opensslForm(want), WantResult: want,
	}, { // Test 4: Surrounding space is trimmed.
		In: "  " + want + "\n", WantResult: want,
	}, { // Test 5: Empty.
		In: "", Want: ErrVerify,
	}, { // Test 6: Too short.
		In: "sha256:abcd", Want: ErrVerify,
	}, { // Test 7: Not hex.
		In: strings.Repeat("zz", 32), Want: ErrVerify,
	}, { // Test 8: Colons in the wrong places.
		In: "0a0:a" + strings.Repeat(":0a", 30) + ":0a", Want: ErrVerify,
	}, { // Test 9: A SHA-1 fingerprint, which is not the digest a pin names.
		In: strings.Repeat("0a", 20), Want: ErrVerify,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := AuthorityPinFingerprint(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("AuthorityPinFingerprint(%q) error = %v, want %v", test.In, err, test.Want)
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("AuthorityPinFingerprint(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// opensslForm writes a "sha256:" fingerprint the way openssl prints one: upper case pairs joined by
// colons.
func opensslForm(fingerprint string) string {
	value := strings.ToUpper(strings.TrimPrefix(fingerprint, "sha256:"))
	pairs := make([]string, 0, len(value)/2)
	for i := 0; i+1 < len(value); i += 2 {
		pairs = append(pairs, value[i:i+2])
	}
	return strings.Join(pairs, ":")
}

// mintAuthority makes a certificate and key named name, issued by issuer, or self-issued when
// issuer is nil. A ca certificate is marked to sign certificates. Any other is a timestamping
// signer.
func mintAuthority(t *testing.T, name string, issuer *authority, ca bool) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate a key for %s: %v", name, err)
	}
	serial, err := cryptorand.Int(cryptorand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("draw a serial for %s: %v", name, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
		BasicConstraintsValid: true,
		IsCA:                  ca,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	if ca {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}
	}
	parent, signer := tmpl, key
	if issuer != nil {
		parent, signer = issuer.Cert, issuer.Key
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatalf("create the certificate for %s: %v", name, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the certificate for %s: %v", name, err)
	}
	return &authority{Cert: cert, Key: key}
}

// tokenBy builds a token over digest signed by auth and carrying the given certificates.
func tokenBy(t *testing.T, digest []byte, auth *authority, carried ...*x509.Certificate) []byte {
	t.Helper()
	sd, err := asn1.Marshal(signedDataBy(t, tstInfoBytes(t, digest, nil), auth, carried...))
	if err != nil {
		t.Fatalf("marshal SignedData: %v", err)
	}
	out, err := asn1.Marshal(tsaContentInfo{
		ContentType: signedDataOID,
		Content:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sd},
	})
	if err != nil {
		t.Fatalf("marshal ContentInfo: %v", err)
	}
	return out
}

// anchoredBundle builds a signed three-entry bundle anchored at its head by the token token makes
// over the head's digest. When published is set it also carries an https anchor at seq 2, which
// carries no token. It returns the bundle and the producer key id.
func anchoredBundle(t *testing.T, published bool, token func(digest []byte) []byte) ([]byte, string) {
	t.Helper()
	entries := buildChain(t, 3)
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	doc, err := BuildBundle(entries, id, "test", time.Now())
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	head := entries[len(entries)-1]
	raw, err := hex.DecodeString(head.Hash)
	if err != nil {
		t.Fatalf("decode the head link: %v", err)
	}
	sum := sha256.Sum256(raw)
	anchors := []*Anchor{{
		ID: "anc_token", Type: AnchorRFC3161, Shape: AnchorShapeLinear, Seq: head.Seq,
		Link: head.Hash, At: genTime(), Ref: "https://tsa.example.com",
		Proof: base64.StdEncoding.EncodeToString(token(sum[:])),
	}}
	if published {
		mid := entries[1]
		anchors = append([]*Anchor{{
			ID: "anc_published", Type: AnchorHTTPS, Shape: AnchorShapeLinear, Seq: mid.Seq,
			Link: mid.Hash, At: genTime(), Ref: "https://anchors.example.com/head",
		}}, anchors...)
	}
	if n := doc.AttachAnchors(anchors); n != len(anchors) {
		t.Fatalf("AttachAnchors kept %d of %d anchors", n, len(anchors))
	}
	signed, err := SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc: %v", err)
	}
	return signed, id.KeyID()
}
