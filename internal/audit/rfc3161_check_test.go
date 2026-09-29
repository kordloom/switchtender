package audit

import (
	"crypto/sha256"
	"encoding/asn1"
	"fmt"
	"math/big"
	"strings"
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

// tokenOver builds a minimal CMS SignedData carrying a TSTInfo over digest, which is the shape an
// authority returns. It is unsigned, because what is being tested is what the token commits to
// rather than who vouched for it.
func tokenOver(t *testing.T, digest []byte) []byte {
	t.Helper()
	return tokenOverTail(t, digest, nil)
}

// tokenOverTail builds the same token with tail appended after genTime, so a test can place the
// optional fields an authority may return there: an accuracy, a nonce, or nothing.
func tokenOverTail(t *testing.T, digest, tail []byte) []byte {
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
	sd := tsaSignedData{
		Version:          3,
		DigestAlgorithms: asn1.RawValue{Class: 0, Tag: 17, IsCompound: true, Bytes: nil},
		EncapContentInfo: tsaEncapContentInfo{EContentType: tstInfoOID, EContent: encoded},
		SignerInfos:      asn1.RawValue{Class: 0, Tag: 17, IsCompound: true, Bytes: nil},
	}
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
