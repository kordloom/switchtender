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
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCheckTimestampTokenRejectsAnUnrelatedToken proves a token that does not commit to the value we
// sent is refused before it can be stored as proof.
//
// The reply used to be checked only for a granted status and a non-empty body, so an authority that
// was hostile, misconfigured, or answering a different question had its answer recorded as an
// anchor. The operator then read "has_proof": true and believed the chain was fixed in time
// somewhere this install cannot rewrite.
func TestCheckTimestampTokenRejectsAnUnrelatedToken(t *testing.T) {
	t.Parallel()
	ours := sha256.Sum256([]byte("the link we asked about"))
	theirs := sha256.Sum256([]byte("something else entirely"))

	tests := []struct {
		Name    string
		Token   []byte
		WantErr string
	}{{ // Test 0: A token committing to a different value.
		Name: "different value", Token: tokenOver(t, theirs[:]),
		WantErr: "attests to a different value",
	}, { // Test 1: Not a token at all, which is what "non-empty body" accepted.
		Name: "not a token", Token: []byte("this token attests to nothing"),
		WantErr: "decode token",
	}, { // Test 2: Empty.
		Name: "empty", Token: nil, WantErr: "decode token",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			err := checkTimestampToken(test.Token, ours[:], nil)
			if err == nil {
				t.Fatalf("a token that does not commit to our value was accepted as proof")
			}
			if !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("error = %v, want it to mention %q", err, test.WantErr)
			}
		})
	}

	// The matching token is accepted, so the check does not refuse honest answers.
	if err := checkTimestampToken(tokenOver(t, ours[:]), ours[:], nil); err != nil {
		t.Errorf("a token committing to our value was refused: %v", err)
	}
}

// TestCheckTimestampTokenComparesTheNonce proves the nonce a token echoes is compared against the
// one that was sent, which is what the doc comment claims it does.
//
// The nonce was sent and never looked at, so a reply recorded off the wire could be handed back as
// the answer to a later request for the same link. Anchoring twice with nothing appended in between
// asks about the same chain head twice, so the imprint alone does not tell the second answer from a
// replay of the first.
func TestCheckTimestampTokenComparesTheNonce(t *testing.T) {
	t.Parallel()
	digest := sha256.Sum256([]byte("the link we asked about"))
	sent := big.NewInt(0x5EED)

	tests := []struct {
		Name    string
		Tail    []byte
		Sent    *big.Int
		WantErr string
	}{{ // Test 0: A replayed reply echoes the nonce of the request it actually answered.
		Name: "replayed nonce", Tail: derInt(t, big.NewInt(0xBEEF)), Sent: sent,
		WantErr: "echoes a different nonce",
	}, { // Test 1: The nonce we sent is accepted.
		Name: "matching nonce", Tail: derInt(t, sent), Sent: sent, WantErr: "",
	}, { // Test 2: An accuracy before the nonce is skipped, not mistaken for it.
		Name: "accuracy then nonce", Tail: append(derAccuracy(t, 1), derInt(t, big.NewInt(0xBEEF))...),
		Sent: sent, WantErr: "echoes a different nonce",
	}, { // Test 3: An authority that echoes nothing is not refused on that ground alone.
		Name: "no nonce echoed", Tail: nil, Sent: sent, WantErr: "",
	}, { // Test 4: Nothing to compare against passes whatever the token echoes.
		Name: "none sent", Tail: derInt(t, big.NewInt(0xBEEF)), Sent: nil, WantErr: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := checkTimestampToken(tokenOverTail(t, digest[:], test.Tail), digest[:], test.Sent)
			if test.WantErr == "" {
				if err != nil {
					t.Fatalf("an honest answer was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a token answering another request was accepted as proof")
			}
			if !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("error = %v, want it to mention %q", err, test.WantErr)
			}
		})
	}
}

// derInt encodes n as a DER INTEGER, the shape a nonce takes inside a TSTInfo.
func derInt(t *testing.T, n *big.Int) []byte {
	t.Helper()
	out, err := asn1.Marshal(n)
	if err != nil {
		t.Fatalf("marshal integer: %v", err)
	}
	return out
}

// derAccuracy encodes an Accuracy of seconds, the optional field an authority may place ahead of
// the nonce.
func derAccuracy(t *testing.T, seconds int) []byte {
	t.Helper()
	out, err := asn1.Marshal(struct {
		Seconds int `asn1:"optional"`
	}{Seconds: seconds})
	if err != nil {
		t.Fatalf("marshal accuracy: %v", err)
	}
	return out
}

// bigOne returns a serial number for a synthesized token.
func bigOne() *big.Int { return big.NewInt(1) }

// genTime returns a fixed generation time for a synthesized token.
func genTime() time.Time { return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC) }

// tokenOver builds a CMS SignedData carrying a TSTInfo over digest, signed by a throwaway authority,
// which is the shape a real authority returns.
//
// It used to build the same structure unsigned, and every test here passed, because nothing on
// either the fetch path or the offline path read SignerInfos. A helper that cannot produce a real
// signature is a helper that cannot tell a real token from a typed one.
func tokenOver(t *testing.T, digest []byte) []byte {
	t.Helper()
	return tokenOverTail(t, digest, nil)
}

// tokenOverTail builds the same token with tail appended after genTime, so a test can place the
// optional fields an authority may return there: an accuracy, a nonce, or nothing.
func tokenOverTail(t *testing.T, digest, tail []byte) []byte {
	t.Helper()
	encoded := tstInfoBytes(t, digest, tail)
	sd := signedDataOver(t, encoded)
	sdBytes, err := asn1.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal SignedData: %v", err)
	}
	ci := tsaContentInfo{
		ContentType: signedDataOID,
		Content:     asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: sdBytes},
	}
	out, err := asn1.Marshal(ci)
	if err != nil {
		t.Fatalf("marshal ContentInfo: %v", err)
	}
	return out
}

// tstInfoBytes marshals the TSTInfo a token signs, which is the part that says what was timestamped.
func tstInfoBytes(t *testing.T, digest, tail []byte) []byte {
	t.Helper()
	info := tsaTSTInfo{
		Version: 1,
		Policy:  asn1.ObjectIdentifier{1, 2, 3, 4},
		MessageImprint: tsaImprint{
			Algorithm: tsaAlgorithm{
				Algorithm:  sha256OID,
				Parameters: asn1.RawValue{Tag: asn1.TagNull},
			},
			Digest: digest,
		},
		SerialNumber: bigOne(),
		GenTime:      genTime(),
		// A RawValue carrying FullBytes is emitted verbatim, so a tail holding more than one
		// element lands in the token exactly as an authority would have written it.
		Rest: asn1.RawValue{FullBytes: tail},
	}
	encoded, err := asn1.Marshal(info)
	if err != nil {
		t.Fatalf("marshal TSTInfo: %v", err)
	}
	return encoded
}

// testAuthority is the throwaway timestamp authority the token helpers sign with, created once so a
// test that builds several tokens does not pay for a key each time.
var testAuthority = sync.OnceValues(newTestAuthority)

// authority is a signing certificate and its key, standing in for a timestamp authority.
type authority struct {
	// Cert is the certificate a token carries so a verifier can check the signature offline.
	Cert *x509.Certificate
	// Key signs the token's attributes.
	Key *ecdsa.PrivateKey
}

// newTestAuthority mints a self-signed certificate to sign test tokens with.
func newTestAuthority() (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4242),
		Subject:      pkix.Name{CommonName: "switchtender test timestamp authority"},
		NotBefore:    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &authority{Cert: cert, Key: key}, nil
}

// signedDataOver wraps a DER TSTInfo in a CMS SignedData signed by the test authority, with the
// content type and message digest attributes a verifier requires.
func signedDataOver(t *testing.T, encoded []byte) tsaSignedData {
	t.Helper()
	auth, err := testAuthority()
	if err != nil {
		t.Fatalf("build the test authority: %v", err)
	}
	sum := sha256.Sum256(encoded)
	typeValue, err := asn1.Marshal(tstInfoOID)
	if err != nil {
		t.Fatalf("marshal content type: %v", err)
	}
	digestValue, err := asn1.Marshal(sum[:])
	if err != nil {
		t.Fatalf("marshal message digest: %v", err)
	}
	attrs := []cmsAttribute{
		{Type: contentTypeAttrOID, Values: asn1.RawValue{
			Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: typeValue}},
		{Type: messageDigestAttrOID, Values: asn1.RawValue{
			Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: digestValue}},
	}
	signedAttrs, err := asn1.Marshal(attrs)
	if err != nil {
		t.Fatalf("marshal signed attributes: %v", err)
	}
	// The signature covers the attributes as a universal SET, which is not the tag they travel
	// under: they arrive as an implicit [0]. Signing the bytes they are marshaled as, rather than
	// the bytes CMS says to sign, produces a token that looks right and verifies nowhere.
	inner, err := innerBytes(signedAttrs)
	if err != nil {
		t.Fatalf("retag signed attributes: %v", err)
	}
	toSign := sha256.Sum256(fullSet(asn1.RawValue{Bytes: inner}))
	sig, err := ecdsa.SignASN1(cryptorand.Reader, auth.Key, toSign[:])
	if err != nil {
		t.Fatalf("sign the token: %v", err)
	}
	info := tsaSignerInfo{
		Version: 1,
		SID: issuerAndSerial{
			Issuer: asn1.RawValue{FullBytes: auth.Cert.RawIssuer},
			Serial: auth.Cert.SerialNumber,
		},
		DigestAlgorithm:    pkix.AlgorithmIdentifier{Algorithm: sha256OID},
		SignedAttrs:        asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: inner},
		SignatureAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidECDSAWithSHA256},
		Signature:          sig,
	}
	infoBytes, err := asn1.Marshal([]tsaSignerInfo{info})
	if err != nil {
		t.Fatalf("marshal signer info: %v", err)
	}
	infoInner, err := innerBytes(infoBytes)
	if err != nil {
		t.Fatalf("retag signer infos: %v", err)
	}
	return tsaSignedData{
		Version:          3,
		DigestAlgorithms: asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true},
		EncapContentInfo: tsaEncapContentInfo{EContentType: tstInfoOID, EContent: encoded},
		Certificates: asn1.RawValue{
			Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: auth.Cert.Raw},
		SignerInfos: asn1.RawValue{
			Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: infoInner},
	}
}

// innerBytes returns the content octets of a DER value, dropping its tag and length so the caller
// can re-tag the same content.
func innerBytes(der []byte) ([]byte, error) {
	var raw asn1.RawValue
	if _, err := asn1.Unmarshal(der, &raw); err != nil {
		return nil, err
	}
	return raw.Bytes, nil
}

// TestTimestampTokenSignatureIsVerified is the check that was missing: nothing anywhere read
// SignerInfos, so a token an operator built themselves passed as third-party evidence.
//
// The consequence was the whole point of an anchor undone. An operator could truncate their chain,
// answer their own timestamp request with an unsigned token over the new head, and from then on
// `switchtender verify` reported an anchor with a timestamp token that fixes the chain, and the
// evidence dossier said the head was fixed somewhere the install cannot rewrite. Each row here is a
// token that used to be accepted.
func TestTimestampTokenSignatureIsVerified(t *testing.T) {
	t.Parallel()
	value := sha256.Sum256([]byte("the head an operator wants to pass off as timestamped"))

	tests := []struct {
		Name    string
		Token   []byte
		WantErr string
	}{{ // Test 0: Nobody signed it. This is the token the old helper built and the old code took.
		Name: "unsigned", Token: unsignedTokenOver(t, value[:]),
		WantErr: "carries no signature",
	}, { // Test 1: Signed by a key that is not the certificate the token presents.
		Name: "signed by another key", Token: forgedSignatureTokenOver(t, value[:]),
		WantErr: "no signer on the token verifies",
	}, { // Test 2: Validly signed, then its payload swapped for one over a different head.
		Name: "payload swapped after signing", Token: swappedPayloadTokenOver(t, value[:]),
		WantErr: "covers a different payload",
	}, { // Test 3: The certificate the signature needs was left out, so nothing can check it.
		Name: "no certificate carried", Token: certlessTokenOver(t, value[:]),
		WantErr: "carries no certificate",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := checkTimestampToken(test.Token, value[:], nil)
			if err == nil {
				t.Fatalf("%s: a token nobody vouched for was accepted as third-party proof",
					test.Name)
			}
			if !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("%s: error = %v, want one containing %q", test.Name, err, test.WantErr)
			}
		})
	}
}

// TestVerifyTimestampProofRefusesAnUnsignedToken pins the same refusal on the offline path, which is
// the one a relying party actually runs. Both paths share the check, and that sharing is why the gap
// existed in both at once.
func TestVerifyTimestampProofRefusesAnUnsignedToken(t *testing.T) {
	t.Parallel()
	const link = "aa00000000000000000000000000000000000000000000000000000000000011"
	raw, err := hex.DecodeString(link)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	sum := sha256.Sum256(raw)
	proof := base64.StdEncoding.EncodeToString(unsignedTokenOver(t, sum[:]))

	if err := VerifyTimestampProof(link, proof); err == nil {
		t.Fatal("a self-made unsigned token verified offline as third-party proof")
	}

	// The honest token still verifies, so the refusal is about the signature and not about the shape.
	good := base64.StdEncoding.EncodeToString(tokenOver(t, sum[:]))
	if err := VerifyTimestampProof(link, good); err != nil {
		t.Errorf("a properly signed token was refused: %v", err)
	}
}

// unsignedTokenOver builds a token carrying an empty SignerInfos set, the shape every token in this
// package used to have.
func unsignedTokenOver(t *testing.T, digest []byte) []byte {
	t.Helper()
	return wrapSignedData(t, func(sd *tsaSignedData) {
		sd.SignerInfos = asn1.RawValue{
			Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true}
		sd.Certificates = asn1.RawValue{}
	}, digest, nil)
}

// certlessTokenOver builds a validly signed token that omits the signing certificate, so a verifier
// offline has nothing to check the signature against.
func certlessTokenOver(t *testing.T, digest []byte) []byte {
	t.Helper()
	return wrapSignedData(t, func(sd *tsaSignedData) {
		sd.Certificates = asn1.RawValue{}
	}, digest, nil)
}

// forgedSignatureTokenOver builds a token whose signature bytes are not the authority's, standing in
// for anyone who edits a token and re-signs with a key of their own.
func forgedSignatureTokenOver(t *testing.T, digest []byte) []byte {
	t.Helper()
	return wrapSignedData(t, func(sd *tsaSignedData) {
		// Flip the last byte of the encoded signer infos, which lands inside the signature value.
		b := append([]byte(nil), sd.SignerInfos.Bytes...)
		b[len(b)-1] ^= 0xFF
		sd.SignerInfos.Bytes = b
	}, digest, nil)
}

// swappedPayloadTokenOver builds a token signed over one head and then carrying another, which is
// the substitution the message-digest attribute exists to catch.
func swappedPayloadTokenOver(t *testing.T, digest []byte) []byte {
	t.Helper()
	other := sha256.Sum256([]byte("a head nobody timestamped"))
	return wrapSignedData(t, nil, other[:], func(sd *tsaSignedData) {
		sd.EncapContentInfo.EContent = tstInfoBytes(t, digest, nil)
	})
}

// wrapSignedData builds a signed token over digest, applies before to the SignedData ahead of
// wrapping and after once it is built, and returns the DER ContentInfo.
func wrapSignedData(t *testing.T, before func(*tsaSignedData), digest []byte,
	after func(*tsaSignedData)) []byte {
	t.Helper()
	sd := signedDataOver(t, tstInfoBytes(t, digest, nil))
	if before != nil {
		before(&sd)
	}
	if after != nil {
		after(&sd)
	}
	sdBytes, err := asn1.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal SignedData: %v", err)
	}
	out, err := asn1.Marshal(tsaContentInfo{
		ContentType: signedDataOID,
		Content:     asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: sdBytes},
	})
	if err != nil {
		t.Fatalf("marshal ContentInfo: %v", err)
	}
	return out
}

// TestTokenOutsideItsSignersValidityIsRefused covers the window check the reference verifier added.
//
// A token whose genTime falls outside its signer's validity was either issued by a certificate that
// had already expired or backdated to before it existed, and either way that certificate does not
// vouch for the instant claimed. Expiry on its own is deliberately not checked: a token is routinely
// read long after its signer's certificate lapsed, which says nothing about when it was issued.
func TestTokenOutsideItsSignersValidityIsRefused(t *testing.T) {
	t.Parallel()
	value := sha256.Sum256([]byte("a head to timestamp"))

	// The test authority is valid 2020 to 2040 and genTime is fixed at 2026, comfortably inside.
	if err := checkTimestampToken(tokenOver(t, value[:]), value[:], nil); err != nil {
		t.Fatalf("a token inside its signer's window was refused: %v", err)
	}

	// Backdated to before the certificate existed, and genuinely signed over that payload so the
	// message-digest check passes and the window check is what refuses it.
	outside := tokenOverInfo(t, tstInfoAt(t, value[:], time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)))
	err := checkTimestampToken(outside, value[:], nil)
	if err == nil {
		t.Fatal("a token dated before its signer existed was accepted")
	}
	if !strings.Contains(err.Error(), "outside its signer's validity") {
		t.Errorf("error = %v, want it to name the validity window", err)
	}
}

// TestATokenWithTwoSignaturesIsRefused matches the reference verifier's rule that a timestamp token
// carries exactly one signer. Taking the first of several that verifies would let a forger attach
// their own beside the authority's and have the token pass on whichever happens to check out.
func TestATokenWithTwoSignaturesIsRefused(t *testing.T) {
	t.Parallel()
	value := sha256.Sum256([]byte("a head to timestamp"))
	doubled := wrapSignedData(t, func(sd *tsaSignedData) {
		// Two copies of the same valid signer info.
		sd.SignerInfos.Bytes = append(append([]byte(nil), sd.SignerInfos.Bytes...),
			sd.SignerInfos.Bytes...)
	}, value[:], nil)

	err := checkTimestampToken(doubled, value[:], nil)
	if err == nil {
		t.Fatal("a token carrying two signatures was accepted")
	}
	if !strings.Contains(err.Error(), "has one") {
		t.Errorf("error = %v, want it to say a token carries one signature", err)
	}
}

// tstInfoAt builds a TSTInfo over digest with an explicit generation time.
func tstInfoAt(t *testing.T, digest []byte, at time.Time) []byte {
	t.Helper()
	info := tsaTSTInfo{
		Version: 1,
		Policy:  asn1.ObjectIdentifier{1, 2, 3, 4},
		MessageImprint: tsaImprint{
			Algorithm: tsaAlgorithm{Algorithm: sha256OID, Parameters: asn1.RawValue{Tag: asn1.TagNull}},
			Digest:    digest,
		},
		SerialNumber: bigOne(),
		GenTime:      at,
	}
	encoded, err := asn1.Marshal(info)
	if err != nil {
		t.Fatalf("marshal TSTInfo: %v", err)
	}
	return encoded
}

// tokenOverInfo signs a prebuilt TSTInfo and wraps it as a ContentInfo, so a test can control the
// payload the signature actually covers rather than swapping it afterward.
func tokenOverInfo(t *testing.T, info []byte) []byte {
	t.Helper()
	sdBytes, err := asn1.Marshal(signedDataOver(t, info))
	if err != nil {
		t.Fatalf("marshal SignedData: %v", err)
	}
	out, err := asn1.Marshal(tsaContentInfo{
		ContentType: signedDataOID,
		Content:     asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: sdBytes},
	})
	if err != nil {
		t.Fatalf("marshal ContentInfo: %v", err)
	}
	return out
}
