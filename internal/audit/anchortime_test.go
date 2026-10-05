package audit

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCheckAnchorTimeRefusesWhatAVerifierWould pins the refusal every anchor writer applies before
// it saves an anchor. A saved anchor reaches every bundle over the range it covers, so one the
// verifier refuses for its time fails every bundle and receipt drawn from the chain afterward, and
// the operator learns why only from a refusal at the moment it was taken.
func TestCheckAnchorTimeRefusesWhatAVerifierWould(t *testing.T) {
	t.Parallel()
	link := hex.EncodeToString(make([]byte, 32))
	raw, err := hex.DecodeString(link)
	if err != nil {
		t.Fatalf("decode the link: %v", err)
	}
	sum := sha256.Sum256(raw)
	gen := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	proof := base64.StdEncoding.EncodeToString(tokenOverInfo(t, tstInfoAt(t, sum[:], gen)))
	tests := []struct {
		Anchor      *Anchor
		EntryAt     time.Time
		WantRefused bool
		Want        error
	}{{ // Test 0: An entry the authority's time follows passes.
		Anchor:  &Anchor{Type: AnchorRFC3161, Seq: 7, Link: link, Proof: proof},
		EntryAt: gen.Add(-time.Minute),
	}, { // Test 1: An entry dated inside the allowed skew after the authority's time passes.
		Anchor:  &Anchor{Type: AnchorRFC3161, Seq: 7, Link: link, Proof: proof},
		EntryAt: gen.Add(AnchorClockSkew),
	}, { // Test 2: An entry dated past the allowed skew after the authority's time is refused.
		Anchor:  &Anchor{Type: AnchorRFC3161, Seq: 7, Link: link, Proof: proof},
		EntryAt: gen.Add(30 * time.Minute), WantRefused: true, Want: ErrAnchorBeforeEntry,
	}, { // Test 3: An anchor checked by fetching its reference carries no token and passes.
		Anchor:  &Anchor{Type: AnchorHTTPS, Seq: 7, Link: link, Ref: "https://example.com/head"},
		EntryAt: gen.Add(30 * time.Minute),
	}, { // Test 4: A token that does not cover the link is refused as unverifiable.
		Anchor: &Anchor{Type: AnchorRFC3161, Seq: 7, Link: hex.EncodeToString([]byte("other")),
			Proof: proof},
		EntryAt: gen, WantRefused: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := CheckAnchorTime(test.Anchor, test.EntryAt)
			if refused := err != nil; refused != test.WantRefused {
				t.Fatalf("CheckAnchorTime() error = %v, want refused %t", err, test.WantRefused)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("CheckAnchorTime() error = %v, want %v", err, test.Want)
			}
		})
	}
}
