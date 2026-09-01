package audit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// TestVerifiesARealTokenFromTheDefaultAuthority covers a token this product did not mint.
//
// Every other test here builds its own token, and a token this package builds is signed the way this
// package expects, so the whole suite agreed with itself while the default authority was unusable.
// freetsa.org, which is the authority `switchtender audit anchor` uses when none is named, answers a
// SHA-256 request with a SHA-512 signature. The payload digest was hardcoded to SHA-256 and the
// signature algorithm table had no SHA-512 entry, so every token it returned was rejected as
// covering a different payload, and anchoring against the default simply did not work.
//
// The fixture is a real response from that authority, kept verbatim. It is the only test here whose
// bytes were produced by somebody else, which is the entire reason it caught this.
func TestVerifiesARealTokenFromTheDefaultAuthority(t *testing.T) {
	t.Parallel()
	token, err := os.ReadFile("testdata/freetsa-sha512-token.der")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	link, err := os.ReadFile("testdata/freetsa-sha512-link.txt")
	if err != nil {
		t.Fatalf("read fixture link: %v", err)
	}
	proof := base64.StdEncoding.EncodeToString(token)
	if err := VerifyTimestampProof(strings.TrimSpace(string(link)), proof); err != nil {
		t.Fatalf("a real token from the default authority was rejected: %v", err)
	}
	when, err := VerifyTimestampProofTime(strings.TrimSpace(string(link)), proof)
	if err != nil {
		t.Fatalf("VerifyTimestampProofTime() error = %v", err)
	}
	if when.IsZero() {
		t.Error("the token verified but reported no time")
	}
}

// TestRealTokenStillFailsAgainstTheWrongLink is the negative half.
//
// Accepting a real token proves the parser reads it. It does not prove the parser checks the token
// is about anything in particular, and a verifier that accepts any well-formed token binds nothing.
func TestRealTokenStillFailsAgainstTheWrongLink(t *testing.T) {
	t.Parallel()
	token, err := os.ReadFile("testdata/freetsa-sha512-token.der")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	proof := base64.StdEncoding.EncodeToString(token)
	other := strings.Repeat("ab", 32)
	if err := VerifyTimestampProof(other, proof); err == nil {
		t.Fatal("a real token verified against a link it does not attest to")
	}
}

// TestRefusesASignerNotAuthorizedToTimestamp covers a token signed by a certificate that carries no
// timestamping extended key usage.
//
// RFC 3161 requires an authority's certificate to be marked for timestamping, and to mark that
// usage critical. Without the check, any certificate a token happens to carry could sign it, so a
// token signed by a certificate its issuer never authorized for this purpose was reported as a
// valid third-party attestation of when something existed. That is the whole claim an anchor makes.
//
// The reference verifier this product's bundles are checked against has always enforced this. This
// one did not, which is the kind of disagreement that only surfaces when somebody runs both.
func TestRefusesASignerNotAuthorizedToTimestamp(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	// Identical to the test authority in every respect except the usage it is authorized for.
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4242),
		Subject:      pkix.Name{CommonName: "switchtender test timestamp authority"},
		NotBefore:    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if findSigner(issuerAndSerial{Serial: cert.SerialNumber,
		Issuer: asn1.RawValue{FullBytes: cert.RawIssuer}},
		[]*x509.Certificate{cert}) != nil {
		t.Fatal("a certificate marked only for code signing was accepted as a timestamp authority")
	}
}

// TestAcceptsASignerThatIsAuthorized is the positive half, so the check above cannot pass by
// refusing everything.
func TestAcceptsASignerThatIsAuthorized(t *testing.T) {
	t.Parallel()
	auth, err := testAuthority()
	if err != nil {
		t.Fatalf("build the test authority: %v", err)
	}
	got := findSigner(issuerAndSerial{Serial: auth.Cert.SerialNumber,
		Issuer: asn1.RawValue{FullBytes: auth.Cert.RawIssuer}},
		[]*x509.Certificate{auth.Cert})
	if got == nil {
		t.Fatal("a certificate marked for timestamping was refused")
	}
}
