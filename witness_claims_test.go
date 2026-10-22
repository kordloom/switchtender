package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The hosted witness's signing key as KordLoom publishes it. docs/witness.md must name exactly this
// key, so a relying party pinning from the repository pins the key the witness signs with.
const (
	// witnessKeyID is the sha256 fingerprint of the witness public key.
	witnessKeyID = "sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804"
	// witnessPublicKey is the raw ed25519 witness public key in standard base64.
	witnessPublicKey = "akNXijRpCjWw1CbuJ3xPWJMHpgghStLfzEd9qwEeFAM="
	// witnessPublicKeyHex is the same key in hex, the other form verify --witness-pubkey takes.
	witnessPublicKeyHex = "6a43578a34690a35b0d426ee277c4f589307a608214ad2dfcc477dab011e1403"
	// witnessValidFrom is when the key began signing.
	witnessValidFrom = "2026-10-22T02:10:33Z"
)

// htmlTag matches one HTML tag, so a sentence on a page matches whether or not a word in it is
// bold or a link.
var htmlTag = regexp.MustCompile(`<[^>]+>`)

// plainText returns html with every tag removed and every whitespace run collapsed to one space.
func plainText(html string) string {
	return whitespaceRun.ReplaceAllString(htmlTag.ReplaceAllString(html, ""), " ")
}

// TestWitnessClaimsStayTrue holds every public sentence about the hosted witness to the wording the
// merged code makes true: the Enterprise card's headline and subhead, the four things the witness
// never receives on both cards that sell it, and the witness page's key id, pins, and lapse terms.
// Card rows are read from that tier's card alone, so a sentence moved to another tier fails.
func TestWitnessClaimsStayTrue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path string
		Card string
		Want string
	}{{ // Test 0: The Enterprise headline.
		Path: "site/pricing.html", Card: "Enterprise",
		Want: "The audit trail has an alibi.",
	}, { // Test 1: The Enterprise subhead.
		Path: "site/pricing.html", Card: "Enterprise",
		Want: "An outside witness countersigns your chain as it grows, so nothing it has seen can " +
			"be rewritten without the verify failing.",
	}, { // Test 2: The Team add-on names the four things the witness never receives.
		Path: "site/pricing.html", Card: "Team",
		Want: "It receives no run content, host names, credentials, or personal data.",
	}, { // Test 3: The Enterprise list names the same four.
		Path: "site/pricing.html", Card: "Enterprise",
		Want: "It receives no run content, host names, credentials, or personal data",
	}, { // Test 4: The witness page names the key id to pin.
		Path: "docs/witness.md",
		Want: "The witness key id is `" + witnessKeyID + "`.",
	}, { // Test 5: The witness page gives the public key in both forms and when it began signing.
		Path: "docs/witness.md",
		Want: "Its public key is `" + witnessPublicKey + "` in base64, which is `" +
			witnessPublicKeyHex + "` in hex, valid from " + witnessValidFrom + ".",
	}, { // Test 6: The enrollment example pins the key.
		Path: "docs/witness.md",
		Want: "--witness-key " + witnessKeyID,
	}, { // Test 7: The verify example pins the key.
		Path: "docs/witness.md",
		Want: "--witness-pubkey " + witnessKeyID,
	}, { // Test 8: Pushes are countersigned through thirty days after the term.
		Path: "docs/witness.md",
		Want: "For thirty days after the term ends the witness still countersigns the install's " +
			"pushes, so a renewal that lands late costs no coverage.",
	}, { // Test 9: Twelve read-only months follow, and the record counts toward the cap.
		Path: "docs/witness.md",
		Want: "For twelve months more the record is read only: the statement and the findings " +
			"stay readable, so an auditor asking about the covered period still gets an answer, " +
			"and the record still counts toward its license id's cap of ten installs. Then the " +
			"record is deleted.",
	}, { // Test 10: A lapse takes no countersignature already issued.
		Path: "docs/witness.md",
		Want: "Every countersignature the install stored, and every export that carries one, keeps " +
			"verifying against the published key.",
	}, { // Test 11: A fresh memory follows deletion, never lapse.
		Path: "docs/witness.md",
		Want: "A fresh memory starts only after the record is deleted, not at lapse.",
	}, { // Test 12: The rendered page carries the key id, so sitegen ran after the page changed.
		Path: "site/docs/witness.html",
		Want: witnessKeyID,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			text := readFlat(t, test.Path)
			where := test.Path
			if test.Card != "" {
				text = plainText(tierCard(t, text, test.Card))
				where = "the " + test.Card + " card on " + test.Path
			}
			if !strings.Contains(text, test.Want) {
				t.Errorf("%s does not say %q", where, test.Want)
			}
		})
	}
}

// TestWitnessPageNamesNoPlaceholderKey fails while docs/witness.md carries a stand-in where a key
// belongs. A relying party pins whatever the page prints, so a placeholder is a key nobody holds.
func TestWitnessPageNamesNoPlaceholderKey(t *testing.T) {
	t.Parallel()
	const path = "docs/witness.md"
	tests := []struct {
		Pattern string
	}{{ // Test 0: An angle-bracket stand-in after the sha256 prefix.
		Pattern: `sha256:<`,
	}, { // Test 1: An ellipsis after the sha256 prefix.
		Pattern: `sha256:\.\.\.`,
	}, { // Test 2: A key id of zeros.
		Pattern: `sha256:0{64}`,
	}, { // Test 3: A word saying the value is not real yet.
		Pattern: `(?i)\b(placeholder|todo|tbd)\b`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if found := regexp.MustCompile(test.Pattern).FindString(readFlat(t, path)); found != "" {
				t.Errorf("%s says %q where the witness key belongs", path, found)
			}
		})
	}
}

// TestWitnessKeyIDIsTheDigestOfItsKey checks that the key id the page pins is the sha256 digest of
// the public key it prints, in both of the forms it prints, so the two cannot drift apart.
func TestWitnessKeyIDIsTheDigestOfItsKey(t *testing.T) {
	t.Parallel()
	fromBase64, err := base64.StdEncoding.DecodeString(witnessPublicKey)
	if err != nil {
		t.Fatalf("decode the base64 witness key: %v", err)
	}
	fromHex, err := hex.DecodeString(witnessPublicKeyHex)
	if err != nil {
		t.Fatalf("decode the hex witness key: %v", err)
	}
	tests := []struct {
		Form string
		Key  []byte
	}{{ // Test 0: The base64 form.
		Form: "base64", Key: fromBase64,
	}, { // Test 1: The hex form.
		Form: "hex", Key: fromHex,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if len(test.Key) != 32 {
				t.Fatalf("the %s witness key is %d bytes, want 32", test.Form, len(test.Key))
			}
			sum := sha256.Sum256(test.Key)
			if got := "sha256:" + hex.EncodeToString(sum[:]); got != witnessKeyID {
				t.Errorf("the %s witness key digests to %s, want %s", test.Form, got, witnessKeyID)
			}
		})
	}
}

// TestWitnessIsNeverCalledIndependent checks that pricing_test.go still forbids calling the witness
// independent, and that the pattern it forbids with still catches the phrase. The witness is
// KordLoom-operated, so it is outside the customer's install and never independent of KordLoom.
func TestWitnessIsNeverCalledIndependent(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("pricing_test.go")
	if err != nil {
		t.Fatalf("read pricing_test.go: %v", err)
	}
	pattern := findOne(t, "Pattern:\\s+`([^`]*independent[^`]*)`", string(raw),
		"the pattern pricing_test.go forbids an independent witness with")
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("compile %q: %v", pattern, err)
	}
	tests := []struct {
		Text string
	}{{ // Test 0: The plain phrase.
		Text: "An independent witness countersigns your chain.",
	}, { // Test 1: The phrase in capitals, as a heading would set it.
		Text: "INDEPENDENT WITNESS",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !re.MatchString(test.Text) {
				t.Errorf("pricing_test.go pattern %q no longer catches %q", pattern, test.Text)
			}
		})
	}
}

// TestWitnessEnrollmentIsOffTheRoadmap fails while the roadmap still lists the enrollment command,
// which ships, so the page never sells as future work what a release already carries.
func TestWitnessEnrollmentIsOffTheRoadmap(t *testing.T) {
	t.Parallel()
	const path = "docs/roadmap.md"
	tests := []struct {
		Pattern string
	}{{ // Test 0: The enrollment command by name.
		Pattern: `(?i)witness enrollment`,
	}, { // Test 1: An install registering for countersigned attestations.
		Pattern: `(?i)registers for countersigned`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if found := regexp.MustCompile(test.Pattern).FindString(readFlat(t, path)); found != "" {
				t.Errorf("%s still lists %q, which ships", path, found)
			}
		})
	}
}
