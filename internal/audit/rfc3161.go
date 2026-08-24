package audit

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"crypto/x509"
	"crypto/x509/pkix"
)

// timestampLimit caps how much of a timestamp authority's reply is read, so a hostile or broken
// server cannot make an anchor consume memory without bound.
const timestampLimit = 1 << 20

// sha256OID identifies SHA-256 in the message imprint sent to a timestamp authority.
var (
	// sha256OID names SHA-256, the digest a timestamp request and its token both use.
	sha256OID = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	// signedDataOID names CMS SignedData, the structure a timestamp token arrives wrapped in.
	signedDataOID = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	// tstInfoOID names TSTInfo, the payload inside a token that says what was timestamped.
	tstInfoOID = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}

	// The algorithms a timestamp authority signs with, in both the combined spelling and the bare
	// key spelling that names its digest separately.
	oidRSAEncryption   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidSHA256WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidRSAPSS          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 10}
	oidECPublicKey     = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidEd25519         = asn1.ObjectIdentifier{1, 3, 101, 112}
)

// tsaAlgorithm is the hash algorithm identifier in a timestamp request.
type tsaAlgorithm struct {
	// Algorithm is the hash OID.
	Algorithm asn1.ObjectIdentifier
	// Parameters is the algorithm's parameters, NULL for SHA-256.
	Parameters asn1.RawValue `asn1:"optional"`
}

// tsaImprint is the hash of the value being timestamped.
type tsaImprint struct {
	// Algorithm names the hash function.
	Algorithm tsaAlgorithm
	// Digest is the hash itself.
	Digest []byte
}

// tsaRequest is an RFC 3161 TimeStampReq.
type tsaRequest struct {
	// Version is the request version, always 1.
	Version int
	// Imprint is the hash being timestamped.
	Imprint tsaImprint
	// Nonce is sent so a conforming authority echoes it, which ties a reply to the request that
	// asked for it. The echo is compared when the token carries one, so a reply recorded off the
	// wire cannot be replayed as the answer to a later request.
	Nonce *big.Int `asn1:"optional"`
	// CertReq asks the authority to include its certificate, which a verifier needs offline.
	CertReq bool `asn1:"optional,default:false"`
}

// tsaStatus is the status member of a TimeStampResp.
type tsaStatus struct {
	// Status is 0 for granted and 1 for granted with modifications; anything else is a refusal.
	Status int
	// StatusString explains a refusal.
	StatusString []string `asn1:"optional"`
	// FailInfo is the refusal's bit string.
	FailInfo asn1.BitString `asn1:"optional"`
}

// tsaResponse is an RFC 3161 TimeStampResp. The token is kept as raw ASN.1 so it is stored exactly
// as the authority signed it.
type tsaResponse struct {
	// Status says whether a token was issued.
	Status tsaStatus
	// Token is the signed timestamp token, opaque here.
	Token asn1.RawValue `asn1:"optional"`
}

// Timestamp asks an RFC 3161 timestamp authority to sign the time it saw link, and returns the
// authority's token as base64.
//
// This is the anchor type worth having. The others fix a link somewhere a relying party has to go
// and fetch, and they prove only that something was published, by someone, at a place that can go
// away. A timestamp token is signed by a third party over the exact link, carries its own
// certificate chain, and is checked offline by standard tooling with no network and no trust in this
// install. That is what turns "the chain has not been altered" into "the chain reached this length
// by this moment, and a stranger attested to it".
//
// A reply is tied to this request two ways. The imprint is the binding one: the stored token is
// checked to commit to the digest that was sent. The nonce is the second, and it is what stops a
// recorded reply being handed back later as a fresh one, since a replayed token echoes the nonce of
// the request it actually answered. It is compared whenever the authority echoes one; a token
// carrying no nonce is not refused on that ground alone, because the imprint already says the reply
// is about this link and the signature that would have to be forged to strip a nonce is checked by
// the relying party reading the finished bundle.
func Timestamp(ctx context.Context, client *http.Client, tsaURL, link string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	raw, err := hex.DecodeString(link)
	if err != nil {
		return "", fmt.Errorf("timestamp: link is not hex: %w", err)
	}
	sum := sha256.Sum256(raw)
	nonce, err := randomNonce()
	if err != nil {
		return "", err
	}
	req := tsaRequest{
		Version: 1,
		Imprint: tsaImprint{
			Algorithm: tsaAlgorithm{
				Algorithm:  sha256OID,
				Parameters: asn1.RawValue{Tag: asn1.TagNull},
			},
			Digest: sum[:],
		},
		Nonce:   nonce,
		CertReq: true,
	}
	body, err := asn1.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("timestamp: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tsaURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("timestamp: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/timestamp-query")
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("timestamp: contact %s: %w", tsaURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("timestamp: %s returned %s", tsaURL, resp.Status)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, timestampLimit))
	if err != nil {
		return "", fmt.Errorf("timestamp: read reply: %w", err)
	}

	var parsed tsaResponse
	if _, err := asn1.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("timestamp: decode reply: %w", err)
	}
	// 0 is granted, 1 is granted with modifications. Anything else means no token was issued, and
	// storing the reply anyway would record an anchor that proves nothing.
	if parsed.Status.Status > 1 {
		return "", fmt.Errorf("timestamp: %s refused: status %d %v",
			tsaURL, parsed.Status.Status, parsed.Status.StatusString)
	}
	if len(parsed.Token.FullBytes) == 0 {
		return "", fmt.Errorf("timestamp: %s granted but returned no token", tsaURL)
	}
	// A granted status and a non-empty body were the only things checked, so a token committing to
	// nothing at all was stored as proof and reported as one.
	if err := checkTimestampToken(parsed.Token.FullBytes, sum[:], nonce); err != nil {
		return "", fmt.Errorf("timestamp: %s: %w", tsaURL, err)
	}
	return base64.StdEncoding.EncodeToString(parsed.Token.FullBytes), nil
}

// VerifyTimestampProof checks a stored RFC 3161 token against the link it claims to fix, offline.
//
// The token was verified once, when it was obtained, and then stored. Nothing read it again: a verifier
// reported an anchor as satisfied because the chain reached the recorded link, and the dossier told the
// auditor the anchor "verifies offline" because a proof string was present. That left the third-party
// half of the claim resting on our own database row, which is the one thing an anchor exists not to rest
// on. Anyone who could edit the anchors table could rewrite the link and leave a proof that commits to
// something else, and every artifact still read as timestamped by an authority.
//
// What this checks is that an authority's token commits to the digest of this link, in the right
// shape, with a SHA-256 imprint, and that the token is really signed by the certificate it carries.
// What it deliberately does not check is whether that certificate chains to an authority worth
// believing: that trust decision belongs to the relying party, who has the token and can hold it
// against the roots their own tooling trusts. Saying so is the point; claiming a check we do not
// perform is what the finding was about, and for a while the unperformed check was the signature.
func VerifyTimestampProof(link, proof string) error {
	if proof == "" {
		return fmt.Errorf("this anchor carries no embedded proof, so there is nothing to verify offline")
	}
	token, err := base64.StdEncoding.DecodeString(proof)
	if err != nil {
		return fmt.Errorf("anchor proof is not base64: %w", err)
	}
	raw, err := hex.DecodeString(link)
	if err != nil {
		return fmt.Errorf("anchor link is not hex: %w", err)
	}
	sum := sha256.Sum256(raw)
	// The nonce is not checked here. It bound one live request to one reply; a stored token is read
	// long afterward, by someone who never made that request.
	if err := checkTimestampToken(token, sum[:], nil); err != nil {
		return fmt.Errorf("anchor proof does not fix this link: %w", err)
	}
	return nil
}

// tsaContentInfo is the CMS wrapper a timestamp token arrives in.
type tsaContentInfo struct {
	// ContentType names the wrapped structure, which for a token is SignedData.
	ContentType asn1.ObjectIdentifier
	// Content is the SignedData itself.
	Content asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

// tsaSignedData is the CMS SignedData a timestamp token carries.
type tsaSignedData struct {
	// Version is the structure version.
	Version int
	// DigestAlgorithms lists the digests used, unread here.
	DigestAlgorithms asn1.RawValue `asn1:"set"`
	// EncapContentInfo holds the TSTInfo being signed.
	EncapContentInfo tsaEncapContentInfo
	// Certificates carries the signer's certificate chain, which the signature check reads.
	Certificates asn1.RawValue `asn1:"optional,tag:0"`
	// CRLs is unread.
	CRLs asn1.RawValue `asn1:"optional,tag:1"`
	// SignerInfos holds the authority's signature.
	SignerInfos asn1.RawValue `asn1:"set"`
}

// tsaEncapContentInfo wraps the signed payload.
type tsaEncapContentInfo struct {
	// EContentType names the payload, which for a token is TSTInfo.
	EContentType asn1.ObjectIdentifier
	// EContent is the DER-encoded TSTInfo.
	EContent []byte `asn1:"explicit,optional,tag:0"`
}

// tsaTSTInfo is the payload a timestamp token signs. It is what says which value was timestamped.
type tsaTSTInfo struct {
	// Version is the structure version.
	Version int
	// Policy is the authority's timestamping policy.
	Policy asn1.ObjectIdentifier
	// MessageImprint is the hash the authority says it timestamped.
	MessageImprint tsaImprint
	// SerialNumber identifies the token at the authority.
	SerialNumber *big.Int
	// GenTime is when the authority says it signed.
	GenTime time.Time `asn1:"generalized"`
	// Rest holds the optional accuracy, ordering, nonce, authority name, and extensions, which are
	// not read here.
	Rest asn1.RawValue `asn1:"optional,any"`
}

// checkTimestampToken confirms a timestamp token commits to the digest that was sent, and that it
// answers this request rather than an earlier one.
//
// Without this the reply was checked only for a granted status and a non-empty body, so an authority
// that was hostile, misconfigured, or simply answering a different question had its answer stored as
// proof. An operator then saw an anchor with a proof attached and believed the chain was fixed in
// time somewhere this install cannot rewrite, and found out otherwise at an audit, which is the
// worst moment to learn it.
//
// Three things are checked, and it is worth being plain about which. The imprint must equal the
// digest that was sent. The nonce must equal the one that was sent whenever the token carries a
// nonce at all; a token without one passes, since the imprint already binds the reply to this link
// and RFC 3161 has a conforming authority echo what it was given. And the token's signature must
// verify under the certificate the token carries.
//
// The signature check used to be skipped, described as leaving the trust decision to the relying
// party. It did not leave a decision to anybody: the offline verifier ran this same function, so
// nothing anywhere read SignerInfos, and a token an operator typed themselves passed as third-party
// evidence. An operator could truncate their chain, answer their own timestamp request, and every
// artifact then reported the head as fixed somewhere the install could not rewrite.
//
// What is still deliberately not checked is the certificate chain: no root store is consulted and
// expiry is not judged, because a token is routinely read long after its certificate expired.
// Whether a given authority is worth believing remains the relying party's call, made against the
// certificate that travels in the bundle.
func checkTimestampToken(token, want []byte, nonce *big.Int) error {
	var ci tsaContentInfo
	if _, err := asn1.Unmarshal(token, &ci); err != nil {
		return fmt.Errorf("decode token: %w", err)
	}
	if !ci.ContentType.Equal(signedDataOID) {
		return fmt.Errorf("token wraps %v, want SignedData", ci.ContentType)
	}
	var sd tsaSignedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return fmt.Errorf("decode signed data: %w", err)
	}
	if !sd.EncapContentInfo.EContentType.Equal(tstInfoOID) {
		return fmt.Errorf("token payload is %v, want TSTInfo", sd.EncapContentInfo.EContentType)
	}
	var info tsaTSTInfo
	if _, err := asn1.Unmarshal(sd.EncapContentInfo.EContent, &info); err != nil {
		return fmt.Errorf("decode TSTInfo: %w", err)
	}
	if !info.MessageImprint.Algorithm.Algorithm.Equal(sha256OID) {
		return fmt.Errorf("token imprint uses %v, want SHA-256",
			info.MessageImprint.Algorithm.Algorithm)
	}
	if !bytes.Equal(info.MessageImprint.Digest, want) {
		return fmt.Errorf("token attests to a different value than the one sent, so it says " +
			"nothing about this chain")
	}
	if got, ok := tokenNonce(sd.EncapContentInfo.EContent); ok && nonce != nil && got.Cmp(nonce) != 0 {
		return fmt.Errorf("token echoes a different nonce than the one sent, so it answers some " +
			"other request and says nothing about when this chain reached here")
	}
	if err := verifyTokenSignature(sd); err != nil {
		return fmt.Errorf("token signature: %w", err)
	}
	return nil
}

// tokenNonce returns the nonce a TSTInfo echoes, and whether it carries one at all.
//
// The nonce sits in the optional tail after genTime, which the named fields stop short of because
// their shapes overlap in ways a struct cannot express: accuracy, ordering, the nonce, the authority
// name, and extensions are each optional, so a decoder that names them has to guess which one a
// given element is. Walking the tail instead needs no guess. The first universal INTEGER after the
// five required fields is the nonce, since accuracy is a SEQUENCE, ordering is a BOOLEAN, and the
// two that follow are context-tagged.
func tokenNonce(tstInfo []byte) (*big.Int, bool) {
	var seq asn1.RawValue
	if _, err := asn1.Unmarshal(tstInfo, &seq); err != nil || !seq.IsCompound {
		return nil, false
	}
	rest := seq.Bytes
	// version, policy, messageImprint, serialNumber, and genTime are required and come first.
	for range 5 {
		var field asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &field); err != nil {
			return nil, false
		}
	}
	for len(rest) > 0 {
		var field asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &field); err != nil {
			return nil, false
		}
		if field.Class != asn1.ClassUniversal || field.Tag != asn1.TagInteger {
			continue
		}
		var n *big.Int
		if _, err := asn1.Unmarshal(field.FullBytes, &n); err != nil || n == nil {
			return nil, false
		}
		return n, true
	}
	return nil, false
}

// randomNonce returns a random nonce for a timestamp request.
func randomNonce() (*big.Int, error) {
	buf := make([]byte, 16)
	if _, err := cryptorand.Read(buf); err != nil {
		return nil, fmt.Errorf("timestamp: nonce: %w", err)
	}
	return new(big.Int).SetBytes(buf), nil
}

// NewAnchor builds an anchor record for a chain coordinate, timestamping it when the type calls
// for one. shape names the coordinate space seq and link live in, linear or tree. The clock is
// passed in so the recorded time is the caller's, matching every other audit time.
//
// installID is the install whose identity the anchored value was computed under. A tree root is
// computed over leaves bound to that identity, so without it a check has no way to tell a chain read
// under a different identity, which is what a restore without its key file and a replica with its own
// key both produce, from a chain that was actually rewritten.
func NewAnchor(ctx context.Context, client *http.Client, kind, ref, shape, installID string,
	seq int64, link string, now time.Time) (*Anchor, error) {
	if !ValidAnchorType(kind) {
		return nil, fmt.Errorf("%w: %q", ErrAnchorType, kind)
	}
	if !ValidAnchorShape(shape) {
		return nil, fmt.Errorf("%w: %q", ErrAnchorShape, shape)
	}
	if seq < 1 || link == "" {
		return nil, fmt.Errorf("anchor: nothing to anchor, the chain is empty")
	}
	a := &Anchor{
		ID: NewAnchorID(), Type: kind, Shape: shape, Seq: seq, Link: link,
		At: now.UTC().Truncate(time.Microsecond), Ref: ref, InstallID: installID,
	}
	if kind != AnchorRFC3161 {
		if ref == "" {
			return nil, fmt.Errorf("anchor: a %s anchor needs a reference a verifier can fetch", kind)
		}
		return a, nil
	}
	proof, err := Timestamp(ctx, client, ref, link)
	if err != nil {
		return nil, err
	}
	a.Proof = proof
	return a, nil
}

// contentTypeAttrOID and messageDigestAttrOID name the two signed attributes RFC 5652 requires a
// signer to cover, and which bind the signature to this payload rather than to any other.
var (
	contentTypeAttrOID   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	messageDigestAttrOID = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
)

// tsaSignerInfo is one CMS SignerInfo: who signed, over what, and with which algorithm.
type tsaSignerInfo struct {
	// Version is 1 for an issuer-and-serial signer identifier.
	Version int
	// SID identifies the signing certificate. Only issuer and serial is read; a token identifying
	// its signer by subject key identifier is refused rather than guessed at.
	SID issuerAndSerial
	// DigestAlgorithm is the hash the signed attributes were taken with.
	DigestAlgorithm pkix.AlgorithmIdentifier
	// SignedAttrs are the attributes actually covered by the signature. RFC 3161 requires them.
	SignedAttrs asn1.RawValue `asn1:"optional,tag:0"`
	// SignatureAlgorithm names how the signature was produced.
	SignatureAlgorithm pkix.AlgorithmIdentifier
	// Signature is the signature value.
	Signature []byte
	// UnsignedAttrs is not covered by the signature and is not read.
	UnsignedAttrs asn1.RawValue `asn1:"optional,tag:1"`
}

// issuerAndSerial identifies a certificate the way CMS does.
type issuerAndSerial struct {
	// Issuer is the DER of the issuer name, compared byte for byte against a candidate certificate.
	Issuer asn1.RawValue
	// Serial is the certificate serial number.
	Serial *big.Int
}

// cmsAttribute is one signed attribute: an OID and its set of values.
type cmsAttribute struct {
	// Type names the attribute.
	Type asn1.ObjectIdentifier
	// Values holds the attribute's values, of which the two this reads carry exactly one.
	Values asn1.RawValue `asn1:"set"`
}

// verifyTokenSignature checks that the authority named in the token actually signed it.
//
// This is the half a relying party cannot supply for itself. Whether a given authority is worth
// believing is their trust decision, made against the certificate in the finished bundle, and this
// deliberately does not make it: no chain is built, no root store is consulted, and expiry is not
// judged, because a token is routinely read long after the certificate that signed it expired.
// What is checked is that the signature verifies under the certificate the token carries, and that
// the signature covers this payload: without that, a token is a value anybody can type, and an
// operator could truncate their own chain and mint an anchor over the new head.
func verifyTokenSignature(sd tsaSignedData) error {
	var infos []tsaSignerInfo
	if _, err := asn1.UnmarshalWithParams(fullSet(sd.SignerInfos), &infos, "set"); err != nil {
		return fmt.Errorf("decode signer infos: %w", err)
	}
	if len(infos) == 0 {
		return fmt.Errorf("the token carries no signature, so nothing vouches for it")
	}
	certs, err := tokenCertificates(sd)
	if err != nil {
		return err
	}
	if len(certs) == 0 {
		return fmt.Errorf("the token carries no certificate, so its signature cannot be checked " +
			"offline. Request the token with the certificate included")
	}
	// One verified signer is what the format needs: a token is signed by one authority, and any
	// signer that does verify is a signer who saw this payload.
	var problems []string
	for _, info := range infos {
		if err := verifyOneSigner(info, sd, certs); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		return nil
	}
	return fmt.Errorf("no signer on the token verifies: %s", strings.Join(problems, "; "))
}

// verifyOneSigner checks a single SignerInfo against the certificates the token carries.
func verifyOneSigner(info tsaSignerInfo, sd tsaSignedData, certs []*x509.Certificate) error {
	cert := findSigner(info.SID, certs)
	if cert == nil {
		return fmt.Errorf("the token names a signer whose certificate it does not carry")
	}
	// RFC 3161 requires signed attributes, and they are what makes the signature specific to this
	// payload. Signing the content directly is permitted by CMS in general but not here, and
	// accepting it would let a signature taken over some other TSTInfo be presented with this one.
	if len(info.SignedAttrs.Bytes) == 0 {
		return fmt.Errorf("the token's signature covers no signed attributes, so it does not bind " +
			"the payload it travels with")
	}
	if err := checkSignedAttrs(info, sd); err != nil {
		return err
	}
	algo, err := signatureAlgorithm(info)
	if err != nil {
		return err
	}
	// The signature is taken over the attributes re-tagged as a SET, which is how CMS says to
	// serialize them for signing even though they travel under an implicit [0].
	return cert.CheckSignature(algo, fullSet(info.SignedAttrs), info.Signature)
}

// checkSignedAttrs confirms the signed attributes name this payload: the content type the token
// declares, and a message digest equal to the hash of the TSTInfo it carries.
func checkSignedAttrs(info tsaSignerInfo, sd tsaSignedData) error {
	var attrs []cmsAttribute
	if _, err := asn1.UnmarshalWithParams(fullSet(info.SignedAttrs), &attrs, "set"); err != nil {
		return fmt.Errorf("decode signed attributes: %w", err)
	}
	var sawType, sawDigest bool
	for _, attr := range attrs {
		switch {
		case attr.Type.Equal(contentTypeAttrOID):
			var got asn1.ObjectIdentifier
			if _, err := asn1.Unmarshal(attr.Values.Bytes, &got); err != nil {
				return fmt.Errorf("decode the signed content type: %w", err)
			}
			if !got.Equal(sd.EncapContentInfo.EContentType) {
				return fmt.Errorf("the signature covers content type %v but the token carries %v",
					got, sd.EncapContentInfo.EContentType)
			}
			sawType = true
		case attr.Type.Equal(messageDigestAttrOID):
			var got []byte
			if _, err := asn1.Unmarshal(attr.Values.Bytes, &got); err != nil {
				return fmt.Errorf("decode the signed message digest: %w", err)
			}
			sum := sha256.Sum256(sd.EncapContentInfo.EContent)
			if !bytes.Equal(got, sum[:]) {
				return fmt.Errorf("the signature covers a different payload than the token carries")
			}
			sawDigest = true
		}
	}
	if !sawType || !sawDigest {
		return fmt.Errorf("the token's signed attributes omit the content type or the message " +
			"digest, so the signature does not bind the payload")
	}
	return nil
}

// signatureAlgorithm maps a signer's algorithm identifiers onto the algorithm x509 verifies with.
//
// A signature algorithm is either spelled out whole, or given as the bare key algorithm with the
// digest named separately, so both spellings are resolved here. Only SHA-256 digests are accepted,
// which is the digest this product requests and the only one the imprint check allows.
func signatureAlgorithm(info tsaSignerInfo) (x509.SignatureAlgorithm, error) {
	switch {
	case info.SignatureAlgorithm.Algorithm.Equal(oidSHA256WithRSA):
		return x509.SHA256WithRSA, nil
	case info.SignatureAlgorithm.Algorithm.Equal(oidECDSAWithSHA256):
		return x509.ECDSAWithSHA256, nil
	case info.SignatureAlgorithm.Algorithm.Equal(oidEd25519):
		return x509.PureEd25519, nil
	case info.SignatureAlgorithm.Algorithm.Equal(oidRSAPSS):
		return x509.SHA256WithRSAPSS, nil
	case info.SignatureAlgorithm.Algorithm.Equal(oidRSAEncryption):
		if !info.DigestAlgorithm.Algorithm.Equal(sha256OID) {
			return 0, fmt.Errorf("the token is signed with digest %v, and only SHA-256 is accepted",
				info.DigestAlgorithm.Algorithm)
		}
		return x509.SHA256WithRSA, nil
	case info.SignatureAlgorithm.Algorithm.Equal(oidECPublicKey):
		if !info.DigestAlgorithm.Algorithm.Equal(sha256OID) {
			return 0, fmt.Errorf("the token is signed with digest %v, and only SHA-256 is accepted",
				info.DigestAlgorithm.Algorithm)
		}
		return x509.ECDSAWithSHA256, nil
	default:
		return 0, fmt.Errorf("the token is signed with algorithm %v, which is not one this reads",
			info.SignatureAlgorithm.Algorithm)
	}
}

// findSigner returns the carried certificate the signer identifier names, or nil when it is absent.
func findSigner(sid issuerAndSerial, certs []*x509.Certificate) *x509.Certificate {
	for _, cert := range certs {
		if cert.SerialNumber.Cmp(sid.Serial) == 0 && bytes.Equal(cert.RawIssuer, sid.Issuer.FullBytes) {
			return cert
		}
	}
	return nil
}

// tokenCertificates parses the certificate set a token carries.
func tokenCertificates(sd tsaSignedData) ([]*x509.Certificate, error) {
	if len(sd.Certificates.Bytes) == 0 {
		return nil, nil
	}
	// The set travels under an implicit [0]; x509 wants the bare concatenated certificates.
	certs, err := x509.ParseCertificates(sd.Certificates.Bytes)
	if err != nil {
		return nil, fmt.Errorf("decode the token's certificates: %w", err)
	}
	return certs, nil
}

// fullSet returns an implicitly tagged constructed value re-tagged as a universal SET, which is the
// encoding CMS signs and the encoding encoding/asn1 will decode into a slice.
func fullSet(v asn1.RawValue) []byte {
	reTagged := asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: v.Bytes}
	out, err := asn1.Marshal(reTagged)
	if err != nil {
		return nil
	}
	return out
}
