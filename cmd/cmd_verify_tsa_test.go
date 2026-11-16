package cmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// The object identifiers a timestamp token is built from.
var (
	// oidTestSHA256 names SHA-256.
	oidTestSHA256 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	// oidTestECDSASHA256 names ECDSA with SHA-256.
	oidTestECDSASHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	// oidTestSignedData names CMS SignedData.
	oidTestSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	// oidTestTSTInfo names the TSTInfo payload.
	oidTestTSTInfo = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	// oidTestContentType names the content type signed attribute.
	oidTestContentType = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	// oidTestMessageDigest names the message digest signed attribute.
	oidTestMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
)

// testTSA is a timestamp authority made inside a test: a certificate nobody vouches for, authorized
// for timestamping, and its key. It is the authority a producer who rewrote their chain would make.
type testTSA struct {
	// Cert is the certificate every token carries.
	Cert *x509.Certificate
	// Key signs every token.
	Key *ecdsa.PrivateKey
}

// testImprint is the hash a timestamp request asks about and a token attests to.
type testImprint struct {
	// Algorithm names the hash.
	Algorithm pkix.AlgorithmIdentifier
	// Digest is the hash.
	Digest []byte
}

// testTSTInfo is the payload a token signs.
type testTSTInfo struct {
	// Version is always 1.
	Version int
	// Policy is the authority's policy.
	Policy asn1.ObjectIdentifier
	// MessageImprint is what was timestamped.
	MessageImprint testImprint
	// SerialNumber identifies the token.
	SerialNumber *big.Int
	// GenTime is when the authority says it signed.
	GenTime time.Time `asn1:"generalized"`
}

// testAttribute is one signed attribute.
type testAttribute struct {
	// Type names the attribute.
	Type asn1.ObjectIdentifier
	// Values holds its one value as a SET.
	Values asn1.RawValue
}

// testIssuerSerial names the signing certificate by issuer and serial.
type testIssuerSerial struct {
	// Issuer is the certificate's issuer name, as DER.
	Issuer asn1.RawValue
	// Serial is the certificate's serial number.
	Serial *big.Int
}

// testSignerInfo is the token's one signature.
type testSignerInfo struct {
	// Version is 1 for an issuer and serial signer identifier.
	Version int
	// SID names the signing certificate.
	SID testIssuerSerial
	// DigestAlgorithm is the hash of the payload in the message digest attribute.
	DigestAlgorithm pkix.AlgorithmIdentifier
	// SignedAttrs are the attributes the signature covers, under an implicit [0].
	SignedAttrs asn1.RawValue
	// SignatureAlgorithm names how the signature was made.
	SignatureAlgorithm pkix.AlgorithmIdentifier
	// Signature is the signature over the attributes.
	Signature []byte
}

// testEncap wraps the signed payload.
type testEncap struct {
	// EContentType names the payload.
	EContentType asn1.ObjectIdentifier
	// EContent is the DER payload.
	EContent []byte `asn1:"explicit,tag:0"`
}

// testSignedData is the CMS SignedData a token is.
type testSignedData struct {
	// Version is the structure version.
	Version int
	// DigestAlgorithms is left empty.
	DigestAlgorithms asn1.RawValue
	// EncapContentInfo holds the TSTInfo.
	EncapContentInfo testEncap
	// Certificates carries the signer's certificate under an implicit [0].
	Certificates asn1.RawValue
	// SignerInfos holds the one signature as a SET.
	SignerInfos asn1.RawValue
}

// testContentInfo is the CMS wrapper a token arrives in.
type testContentInfo struct {
	// ContentType names SignedData.
	ContentType asn1.ObjectIdentifier
	// Content is the SignedData under an explicit [0].
	Content asn1.RawValue
}

// newTestTSA makes a timestamp authority with a self-issued certificate.
func newTestTSA(t *testing.T) *testTSA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate the authority key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject: pkix.Name{CommonName: "made-on-the-spot timestamp authority",
			Organization: []string{"Nobody You Trust"}},
		NotBefore:   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:    time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the authority certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the authority certificate: %v", err)
	}
	return &testTSA{Cert: cert, Key: key}
}

// sign returns a DER timestamp token over digest, dated at, signed by the authority and carrying
// its certificate.
func (a *testTSA) sign(digest []byte, at time.Time) ([]byte, error) {
	info, err := asn1.Marshal(testTSTInfo{
		Version: 1, Policy: asn1.ObjectIdentifier{1, 2, 3, 4},
		MessageImprint: testImprint{
			Algorithm: pkix.AlgorithmIdentifier{Algorithm: oidTestSHA256}, Digest: digest},
		SerialNumber: big.NewInt(1), GenTime: at.UTC().Truncate(time.Second),
	})
	if err != nil {
		return nil, err
	}
	typeValue, err := asn1.Marshal(oidTestTSTInfo)
	if err != nil {
		return nil, err
	}
	payloadSum := sha256.Sum256(info)
	digestValue, err := asn1.Marshal(payloadSum[:])
	if err != nil {
		return nil, err
	}
	attrs, err := asn1.Marshal([]testAttribute{
		{Type: oidTestContentType, Values: derSet(typeValue)},
		{Type: oidTestMessageDigest, Values: derSet(digestValue)},
	})
	if err != nil {
		return nil, err
	}
	attrsInner, err := derContent(attrs)
	if err != nil {
		return nil, err
	}
	// The signature covers the attributes as a universal SET, though they travel under [0].
	toSign, err := asn1.Marshal(derSet(attrsInner))
	if err != nil {
		return nil, err
	}
	attrsSum := sha256.Sum256(toSign)
	sig, err := ecdsa.SignASN1(cryptorand.Reader, a.Key, attrsSum[:])
	if err != nil {
		return nil, err
	}
	infos, err := asn1.Marshal([]testSignerInfo{{
		Version: 1,
		SID: testIssuerSerial{
			Issuer: asn1.RawValue{FullBytes: a.Cert.RawIssuer}, Serial: a.Cert.SerialNumber},
		DigestAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidTestSHA256},
		SignedAttrs: asn1.RawValue{
			Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: attrsInner},
		SignatureAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidTestECDSASHA256},
		Signature:          sig,
	}})
	if err != nil {
		return nil, err
	}
	infosInner, err := derContent(infos)
	if err != nil {
		return nil, err
	}
	sd, err := asn1.Marshal(testSignedData{
		Version:          3,
		DigestAlgorithms: derSet(nil),
		EncapContentInfo: testEncap{EContentType: oidTestTSTInfo, EContent: info},
		Certificates: asn1.RawValue{
			Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: a.Cert.Raw},
		SignerInfos: derSet(infosInner),
	})
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(testContentInfo{
		ContentType: oidTestSignedData,
		Content: asn1.RawValue{
			Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sd},
	})
}

// serve answers RFC 3161 requests with tokens this authority signs at the time of the request.
func (a *testTSA) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			Version int
			Imprint testImprint
		}
		if _, err := asn1.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		token, err := a.sign(req.Imprint.Digest, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply, err := asn1.Marshal(struct {
			Status struct{ Status int }
			Token  asn1.RawValue
		}{Token: asn1.RawValue{FullBytes: token}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write(reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// derSet wraps content octets as a universal SET.
func derSet(inner []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: inner}
}

// derContent returns the content octets of a DER value, without its tag and length.
func derContent(der []byte) ([]byte, error) {
	var raw asn1.RawValue
	if _, err := asn1.Unmarshal(der, &raw); err != nil {
		return nil, err
	}
	return raw.Bytes, nil
}

// writeAnchoredBundle seeds a finished run, anchors the chain head at tsa through `audit anchor`,
// writes the whole-install bundle, and returns the bundle's path and the producer key id.
func writeAnchoredBundle(t *testing.T, tsa *testTSA) (path, keyID string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	seedReceiptableRun(t, db, "run_stamped")
	setString(t, &anchorDB, db)
	setString(t, &anchorType, audit.AnchorRFC3161)
	setString(t, &anchorRef, tsa.serve(t).URL)
	setBool(t, &anchorTree, false)
	if err := runAuditAnchor(testCommand(), nil); err != nil {
		t.Fatalf("runAuditAnchor() error = %v", err)
	}
	path = filepath.Join(dir, "audit.loomseal.json")
	setString(t, &bundleDB, db)
	setString(t, &bundleOut, path)
	if err := runAuditBundle(testCommand(), nil); err != nil {
		t.Fatalf("runAuditBundle() error = %v", err)
	}
	signed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(bundle) error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle(bundle) error = %v", err)
	}
	if rep.AnchorCount != 1 || rep.TimestampsVerified != 1 {
		t.Fatalf("the bundle carries %d anchors and %d verified tokens, want 1 of each",
			rep.AnchorCount, rep.TimestampsVerified)
	}
	return path, rep.KeyID
}

// TestVerifyCountsATimestampTokenOnlyWhenItsAuthorityIsPinned pins what verify says about a
// timestamp token from an authority made on the spot. The token verifies, because it is checked
// against the certificate it carries, so counting it as fixing the chain with no signer named would
// leave a reader nothing to check it against. With no --tsa-cert verify prints the signer's subject
// and fingerprint, reads UNPINNED, counts the anchor out, and keeps the verdict and the exit code a
// good bundle gets. Pinned to that certificate, in any form a reader copies, the anchor counts.
// Pinned to another, verify fails and names the signer that did not match.
func TestVerifyCountsATimestampTokenOnlyWhenItsAuthorityIsPinned(t *testing.T) {
	// Not parallel: the audit commands and verify read package-level flag variables.
	tsa := newTestTSA(t)
	bundle, keyID := writeAnchoredBundle(t, tsa)
	sum := sha256.Sum256(tsa.Cert.Raw)
	own := "sha256:" + hex.EncodeToString(sum[:])
	colons := make([]string, 0, len(sum))
	for _, b := range sum {
		colons = append(colons, fmt.Sprintf("%02X", b))
	}
	other := "sha256:" + strings.Repeat("ab", 32)
	signerLines := []string{
		"    signer       CN=made-on-the-spot timestamp authority,O=Nobody You Trust\n",
		"    fingerprint  " + own + "\n",
	}
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Pins are the --tsa-cert values.
		Pins []string
		// Pubkey is the --pubkey value.
		Pubkey string
		// WantLines must all appear in the output.
		WantLines []string
		// DenyLines must not.
		DenyLines []string
		// WantToken matches the token's state line.
		WantToken string
		// WantErr must appear in the error, empty when verify succeeds.
		WantErr string
		// WantCode is the exit code.
		WantCode int
	}{{ // Test 0: No pin, the token is named and UNPINNED, and the verdict is what it was.
		Name: "no pin",
		WantLines: []string{"anchors      UNPINNED (0 of 1 counted: no timestamp authority was " +
			"pinned, so nothing ties the signer below to one you trust)",
			"\nINTACT, BUT UNIDENTIFIED:"},
		DenyLines: []string{"anchors      OK", "fixes this chain", "yours to check", "pinned, counted"},
		WantToken: "UNPINNED, not counted", WantCode: CodeOK,
	}, { // Test 1: No authority pin with the producer pinned, the verdict is VERIFIED as before.
		Name: "producer pinned, no authority pin", Pubkey: keyID,
		WantLines: []string{"anchors      UNPINNED (0 of 1 counted", "\nVERIFIED:"},
		WantToken: "UNPINNED, not counted", WantCode: CodeOK,
	}, { // Test 2: The authority pinned by its own fingerprint, the anchor counts.
		Name: "authority pinned", Pins: []string{own}, Pubkey: keyID,
		WantLines: []string{"anchors      OK (1 of 1 counted)\n", "\nVERIFIED:"},
		DenyLines: []string{"UNPINNED"},
		WantToken: "signer pinned, counted", WantCode: CodeOK,
	}, { // Test 3: The same pin as openssl prints it.
		Name: "authority pinned in the openssl form", Pins: []string{strings.Join(colons, ":")},
		Pubkey:    keyID,
		WantLines: []string{"anchors      OK (1 of 1 counted)\n", "\nVERIFIED:"},
		WantToken: "signer pinned, counted", WantCode: CodeOK,
	}, { // Test 4: Another authority pinned, verify fails and names the signer that did not match.
		Name: "another authority pinned", Pins: []string{other}, Pubkey: keyID,
		WantLines: []string{"anchors      FAILED (0 of 1 counted: 1 token is signed by no " +
			"timestamp authority you pinned)",
			"its token is signed by CN=made-on-the-spot timestamp authority,O=Nobody You Trust (" +
				own + "), which matches no timestamp authority you pinned",
			"\nNOT VERIFIED: a timestamp token is signed by no timestamp authority you pinned"},
		DenyLines: []string{"\nVERIFIED:", "UNPINNED"},
		WantToken: "matches no timestamp authority you pinned, FAILED",
		WantErr:   "a timestamp token is signed by no timestamp authority you pinned",
		WantCode:  CodeError,
	}, { // Test 5: A pin that is not a fingerprint is refused, never read as no pin.
		Name: "malformed pin", Pins: []string{"freetsa.org"}, Pubkey: keyID,
		WantErr: "is not a SHA-256 certificate fingerprint", WantCode: CodeError,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			setString(t, &verifyPubkey, test.Pubkey)
			old := verifyTSACerts
			t.Cleanup(func() { verifyTSACerts = old })
			verifyTSACerts = test.Pins
			var buf bytes.Buffer
			c := testCommand()
			c.SetOut(&buf)
			err := runVerify(c, []string{bundle})
			got := buf.String()
			switch {
			case test.WantErr == "" && err != nil:
				t.Fatalf("%s: runVerify() error = %v\n%s", test.Name, err, got)
			case test.WantErr != "" && (err == nil || !strings.Contains(err.Error(), test.WantErr)):
				t.Fatalf("%s: runVerify() error = %v, want %q\n%s", test.Name, err, test.WantErr, got)
			}
			want := test.WantLines
			if test.WantToken != "" {
				want = append(append([]string(nil), want...), signerLines...)
			}
			for _, line := range want {
				if !strings.Contains(got, line) {
					t.Errorf("%s: output is missing %q:\n%s", test.Name, line, got)
				}
			}
			for _, deny := range test.DenyLines {
				if strings.Contains(got, deny) {
					t.Errorf("%s: output carries %q:\n%s", test.Name, deny, got)
				}
			}
			if test.WantToken != "" {
				state := regexp.MustCompile(`(?m)^  token at seq \d+, ` +
					regexp.QuoteMeta(test.WantToken) + `$`)
				if !state.MatchString(got) {
					t.Errorf("%s: output does not label the token %q:\n%s", test.Name,
						test.WantToken, got)
				}
			}

			// The exit code is what a script branches on. An unpinned authority must not move it,
			// and a mismatched one must.
			args := []string{"verify", bundle}
			if test.Pubkey != "" {
				args = append(args, "--pubkey", test.Pubkey)
			}
			for _, pin := range test.Pins {
				args = append(args, "--tsa-cert", pin)
			}
			stdout, stderr, code := runCLI(t, args...)
			if code != test.WantCode {
				t.Fatalf("%s: switchtender %q exited %d, want %d.\nstdout:\n%s\nstderr:\n%s",
					test.Name, args, code, test.WantCode, stdout, stderr)
			}
			if len(test.WantLines) > 0 && !strings.Contains(stdout, test.WantLines[0]) {
				t.Errorf("%s: the CLI output differs from the in-process run:\n%s", test.Name, stdout)
			}
		})
	}
}

// renderDossier writes the dossier of runID from db through `audit run` and returns its HTML.
func renderDossier(t *testing.T, db, runID string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "dossier.html")
	setString(t, &dossierDB, db)
	setString(t, &dossierOut, out)
	if err := runAuditRunDossier(testCommand(), []string{runID}); err != nil {
		t.Fatalf("runAuditRunDossier(%s) error = %v", runID, err)
	}
	doc, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile(dossier) error = %v", err)
	}
	return string(doc)
}

// verifyOutput runs verify on a bundle in process and through the CLI, and returns the in-process
// output, the CLI exit code, and the in-process error.
func verifyOutput(t *testing.T, bundle string) (out string, code int, err error) {
	t.Helper()
	setString(t, &verifyPubkey, "")
	old := verifyTSACerts
	t.Cleanup(func() { verifyTSACerts = old })
	verifyTSACerts = nil
	var buf bytes.Buffer
	c := testCommand()
	c.SetOut(&buf)
	err = runVerify(c, []string{bundle})
	_, _, code = runCLI(t, "verify", bundle)
	return buf.String(), code, err
}

// TestDossierAndVerifyNameOneTimestampToken pins that the run dossier and verify describe one
// timestamp token in the same words. A token is checked against the certificate it carries, and
// anyone can make one, so the dossier names its signer, counts nothing, and says nothing fixes the
// run's history. It prints the bundle command that lets verify count the token once an authority is
// pinned, and that command has to work: the bundle it writes carries the same token, and verify
// prints the same fingerprint and the same state words. An anchor by reference is named as not
// fetched, in the dossier and in verify, which leaves it out of the count with the verdict and the
// exit code unchanged.
func TestDossierAndVerifyNameOneTimestampToken(t *testing.T) {
	// Not parallel: the audit commands and verify read package-level flag variables.
	tsa := newTestTSA(t)
	whole, _ := writeAnchoredBundle(t, tsa)
	db := filepath.Join(filepath.Dir(whole), "state.db")
	sum := sha256.Sum256(tsa.Cert.Raw)
	own := "sha256:" + hex.EncodeToString(sum[:])

	html := renderDossier(t, db, "run_stamped")
	for _, want := range []string{`<div class="banner anchored">`, own, "UNPINNED, not counted",
		"names its signer CN=made-on-the-spot timestamp authority,O=Nobody You Trust",
		"1 carries a timestamp token that is unpinned and not counted."} {
		if !strings.Contains(html, want) {
			t.Errorf("the dossier is missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(strings.ToLower(html), "fix") {
		t.Errorf("the dossier says an anchor fixes something:\n%s", html)
	}

	// The bundle command the dossier prints writes a bundle verify reads the same token from.
	m := regexp.MustCompile(`id="bundle-command">switchtender audit bundle --from-seq (\d+) ` +
		`--to-seq (\d+)<`).FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("the dossier prints no bundle command:\n%s", html)
	}
	from, _ := strconv.ParseInt(m[1], 10, 64)
	to, _ := strconv.ParseInt(m[2], 10, 64)
	oldFrom, oldTo := bundleFromSeq, bundleToSeq
	t.Cleanup(func() { bundleFromSeq, bundleToSeq = oldFrom, oldTo })
	bundleFromSeq, bundleToSeq = from, to
	setInt(t, &bundleLimit, 0)
	setString(t, &bundleDB, db)
	windowed := filepath.Join(t.TempDir(), "window.loomseal.json")
	setString(t, &bundleOut, windowed)
	if err := runAuditBundle(testCommand(), nil); err != nil {
		t.Fatalf("the printed bundle command failed: --from-seq %d --to-seq %d: %v", from, to, err)
	}
	bundleFromSeq, bundleToSeq = 0, 0
	got, code, err := verifyOutput(t, windowed)
	if err != nil || code != CodeOK {
		t.Fatalf("verify on the printed bundle: error = %v, exit %d, want none and %d:\n%s", err,
			code, CodeOK, got)
	}
	state := regexp.MustCompile(`(?m)^  token at seq \d+, UNPINNED, not counted$`)
	if !state.MatchString(got) || !strings.Contains(got, "    fingerprint  "+own+"\n") {
		t.Errorf("verify does not name the token the dossier names (%s, UNPINNED, not counted):\n%s",
			own, got)
	}

	// An anchor by reference alone: the dossier says it was not fetched, and verify leaves it out
	// of the count with the verdict and the exit code a good bundle gets.
	dir := t.TempDir()
	refDB := filepath.Join(dir, "state.db")
	seedReceiptableRun(t, refDB, "run_published")
	setString(t, &anchorDB, refDB)
	setString(t, &anchorType, audit.AnchorHTTPS)
	setString(t, &anchorRef, "https://example.com/switchtender/head")
	setBool(t, &anchorTree, false)
	if err := runAuditAnchor(testCommand(), nil); err != nil {
		t.Fatalf("runAuditAnchor(https) error = %v", err)
	}
	html = renderDossier(t, refDB, "run_published")
	for _, want := range []string{`<div class="banner anchored">`, "not fetched here",
		"1 is by reference and rests on where it was published."} {
		if !strings.Contains(html, want) {
			t.Errorf("the by-reference dossier is missing %q:\n%s", want, html)
		}
	}
	published := filepath.Join(dir, "audit.loomseal.json")
	setString(t, &bundleDB, refDB)
	setString(t, &bundleOut, published)
	if err := runAuditBundle(testCommand(), nil); err != nil {
		t.Fatalf("runAuditBundle(by reference) error = %v", err)
	}
	got, code, err = verifyOutput(t, published)
	if err != nil || code != CodeOK {
		t.Fatalf("verify on a by-reference anchor: error = %v, exit %d, want none and %d:\n%s",
			err, code, CodeOK, got)
	}
	for _, want := range []string{"anchors      OK (0 of 1 counted)\n",
		"  1 with no timestamp token, not fetched here and not counted: its position rests on " +
			"where it was published\n", "\nINTACT, BUT UNIDENTIFIED:"} {
		if !strings.Contains(got, want) {
			t.Errorf("verify on a by-reference anchor is missing %q:\n%s", want, got)
		}
	}
}
