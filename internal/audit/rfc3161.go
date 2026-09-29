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
var sha256OID = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}

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
	// Nonce ties this reply to this request, so a recorded reply cannot be replayed at us.
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
// The nonce is what stops a recorded reply being handed back to us later as a fresh one.
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
	return base64.StdEncoding.EncodeToString(parsed.Token.FullBytes), nil
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
