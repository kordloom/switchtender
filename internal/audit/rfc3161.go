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
	"time"
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

// tsaContentInfo is the CMS wrapper a timestamp token arrives in.
type tsaContentInfo struct {
	// ContentType names the wrapped structure, which for a token is SignedData.
	ContentType asn1.ObjectIdentifier
	// Content is the SignedData itself.
	Content asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

// tsaSignedData is the CMS SignedData a timestamp token carries. Only the parts needed to read what
// the token commits to are named; the signature is checked by a verifier reading the finished
// bundle, offline and with no trust in this install.
type tsaSignedData struct {
	// Version is the structure version.
	Version int
	// DigestAlgorithms lists the digests used, unread here.
	DigestAlgorithms asn1.RawValue `asn1:"set"`
	// EncapContentInfo holds the TSTInfo being signed.
	EncapContentInfo tsaEncapContentInfo
	// Certificates carries the signer's certificate chain, unread here.
	Certificates asn1.RawValue `asn1:"optional,tag:0"`
	// CRLs is unread.
	CRLs asn1.RawValue `asn1:"optional,tag:1"`
	// SignerInfos holds the authority's signature, unread here.
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
// Exactly two things are checked, and it is worth being plain about which. The imprint must equal
// the digest that was sent. The nonce must equal the one that was sent whenever the token carries a
// nonce at all; a token without one passes, since the imprint already binds the reply to this link
// and RFC 3161 has a conforming authority echo what it was given.
//
// The signature is deliberately not checked here. Whether the authority is worth trusting is the
// relying party's call, made offline against the token in the finished bundle, and a check made here
// would only be this install vouching for itself. What must be true before storing it is narrower:
// this token is about this link, and it was issued for this request.
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

// NewAnchor builds an anchor record for a chain head, timestamping it when the type calls for one.
// The clock is passed in so the recorded time is the caller's, matching every other audit time.
func NewAnchor(ctx context.Context, client *http.Client, kind, ref string,
	seq int64, link string, now time.Time) (*Anchor, error) {
	if !ValidAnchorType(kind) {
		return nil, fmt.Errorf("%w: %q", ErrAnchorType, kind)
	}
	if seq < 1 || link == "" {
		return nil, fmt.Errorf("anchor: nothing to anchor, the chain is empty")
	}
	a := &Anchor{
		ID: NewAnchorID(), Type: kind, Seq: seq, Link: link,
		At: now.UTC().Truncate(time.Microsecond), Ref: ref,
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
