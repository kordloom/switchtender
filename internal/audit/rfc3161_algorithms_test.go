package audit

import (
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"testing"
)

// TestSignatureAlgorithmResolvesEverySpellingItClaims covers the algorithm table itself.
//
// Only one algorithm is reachable through the real-token fixture, which is whichever one the default
// authority happens to sign with. Every other row was added on the strength of reading a spec, and a
// mutation run showed each of their identifiers could be altered without a single test noticing. An
// authority signing with any of them would then be reported as unreadable, which is the same failure
// that made the default authority unusable in the first place, one row over.
func TestSignatureAlgorithmResolvesEverySpellingItClaims(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		SigAlg asn1.ObjectIdentifier
		Digest asn1.ObjectIdentifier
		Want   x509.SignatureAlgorithm
	}{ // Combined spellings, where the algorithm names its own digest.
		{"rsa-sha256", oidSHA256WithRSA, sha256OID, x509.SHA256WithRSA},
		{"rsa-sha384", oidSHA384WithRSA, sha384OID, x509.SHA384WithRSA},
		{"rsa-sha512", oidSHA512WithRSA, sha512OID, x509.SHA512WithRSA},
		{"ecdsa-sha256", oidECDSAWithSHA256, sha256OID, x509.ECDSAWithSHA256},
		{"ecdsa-sha384", oidECDSAWithSHA384, sha384OID, x509.ECDSAWithSHA384},
		{"ecdsa-sha512", oidECDSAWithSHA512, sha512OID, x509.ECDSAWithSHA512},
		{"ed25519", oidEd25519, sha512OID, x509.PureEd25519},
		// Bare key spellings, where the digest comes from the signer info instead.
		{"bare-rsa-sha256", oidRSAEncryption, sha256OID, x509.SHA256WithRSA},
		{"bare-rsa-sha384", oidRSAEncryption, sha384OID, x509.SHA384WithRSA},
		{"bare-rsa-sha512", oidRSAEncryption, sha512OID, x509.SHA512WithRSA},
		{"bare-ec-sha256", oidECPublicKey, sha256OID, x509.ECDSAWithSHA256},
		{"bare-ec-sha384", oidECPublicKey, sha384OID, x509.ECDSAWithSHA384},
		{"bare-ec-sha512", oidECPublicKey, sha512OID, x509.ECDSAWithSHA512},
		{"pss-sha256", oidRSAPSS, sha256OID, x509.SHA256WithRSAPSS},
		{"pss-sha384", oidRSAPSS, sha384OID, x509.SHA384WithRSAPSS},
		{"pss-sha512", oidRSAPSS, sha512OID, x509.SHA512WithRSAPSS},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			info := tsaSignerInfo{}
			info.SignatureAlgorithm.Algorithm = test.SigAlg
			info.DigestAlgorithm.Algorithm = test.Digest
			got, err := signatureAlgorithm(info)
			if err != nil {
				t.Fatalf("signatureAlgorithm() error = %v, want %v", err, test.Want)
			}
			if got != test.Want {
				t.Errorf("signatureAlgorithm() = %v, want %v", got, test.Want)
			}
		})
	}
}

// TestSignatureAlgorithmRefusesWhatItCannotRead is the closed half.
//
// A table that resolves everything is not a table, it is a default. An identifier this build does
// not know must be refused by name rather than resolved to a neighbor, because verifying a signature
// with the wrong algorithm is not a near miss.
func TestSignatureAlgorithmRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		SigAlg asn1.ObjectIdentifier
		Digest asn1.ObjectIdentifier
	}{
		{"unknown signature algorithm", asn1.ObjectIdentifier{1, 2, 3, 4}, sha256OID},
		{"rsa with an unreadable digest", oidRSAEncryption, asn1.ObjectIdentifier{1, 2, 3, 4}},
		{"ec with an unreadable digest", oidECPublicKey, asn1.ObjectIdentifier{1, 2, 3, 4}},
		{"pss with an unreadable digest", oidRSAPSS, asn1.ObjectIdentifier{1, 2, 3, 4}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			info := tsaSignerInfo{}
			info.SignatureAlgorithm.Algorithm = test.SigAlg
			info.DigestAlgorithm.Algorithm = test.Digest
			if got, err := signatureAlgorithm(info); err == nil {
				t.Errorf("signatureAlgorithm() = %v, want a refusal", got)
			}
		})
	}
}

// TestDigestHashReadsEveryDigestTheTableNames covers the payload digest resolution beside it.
//
// This is the function whose hardcoded SHA-256 made the default authority unusable. Every entry that
// replaced the hardcoding needs a test, or the next one to be wrong is found the same way: by a user.
func TestDigestHashReadsEveryDigestTheTableNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		OID  asn1.ObjectIdentifier
		Want crypto.Hash
	}{
		{sha256OID, crypto.SHA256},
		{sha384OID, crypto.SHA384},
		{sha512OID, crypto.SHA512},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := digestHash(test.OID)
			if err != nil {
				t.Fatalf("digestHash() error = %v", err)
			}
			if got != test.Want {
				t.Errorf("digestHash() = %v, want %v", got, test.Want)
			}
		})
	}
	if _, err := digestHash(asn1.ObjectIdentifier{1, 2, 3, 4}); err == nil {
		t.Error("digestHash() accepted an identifier it does not know")
	}
}

// TestObjectIdentifiersMatchTheirPublishedValues pins each identifier against the number the
// standards actually assign, written out here independently of the constant.
//
// A test that resolves an algorithm by naming its constant cannot catch a wrong constant: the test
// reads the same symbol the code does, so both move together and the test still passes. A mutation
// run proved exactly that, changing digits in these identifiers without a single failure. The only
// thing that catches a typo here is a second, independent spelling of the value, which is what this
// is. A wrong identifier is not a crash; it is a token from a conforming authority reported as
// unreadable, or worse, one algorithm's signature checked as another's.
func TestObjectIdentifiersMatchTheirPublishedValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Got  asn1.ObjectIdentifier
		Want string
	}{
		{"id-sha256", sha256OID, "2.16.840.1.101.3.4.2.1"},
		{"id-sha384", sha384OID, "2.16.840.1.101.3.4.2.2"},
		{"id-sha512", sha512OID, "2.16.840.1.101.3.4.2.3"},
		{"sha256WithRSAEncryption", oidSHA256WithRSA, "1.2.840.113549.1.1.11"},
		{"sha384WithRSAEncryption", oidSHA384WithRSA, "1.2.840.113549.1.1.12"},
		{"sha512WithRSAEncryption", oidSHA512WithRSA, "1.2.840.113549.1.1.13"},
		{"rsaEncryption", oidRSAEncryption, "1.2.840.113549.1.1.1"},
		{"id-RSASSA-PSS", oidRSAPSS, "1.2.840.113549.1.1.10"},
		{"id-ecPublicKey", oidECPublicKey, "1.2.840.10045.2.1"},
		{"ecdsa-with-SHA256", oidECDSAWithSHA256, "1.2.840.10045.4.3.2"},
		{"ecdsa-with-SHA384", oidECDSAWithSHA384, "1.2.840.10045.4.3.3"},
		{"ecdsa-with-SHA512", oidECDSAWithSHA512, "1.2.840.10045.4.3.4"},
		{"id-Ed25519", oidEd25519, "1.3.101.112"},
		{"id-signedData", signedDataOID, "1.2.840.113549.1.7.2"},
		{"id-ct-TSTInfo", tstInfoOID, "1.2.840.113549.1.9.16.1.4"},
		{"id-contentType", contentTypeAttrOID, "1.2.840.113549.1.9.3"},
		{"id-messageDigest", messageDigestAttrOID, "1.2.840.113549.1.9.4"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := test.Got.String(); got != test.Want {
				t.Errorf("%s = %s, want %s", test.Name, got, test.Want)
			}
		})
	}
}
