package audit

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/kordloom/loomseal/jcs"
)

// TestCanonicalMatchesTheSharedSerializer pins that this package's canonicalization and LoomSeal's
// exported one produce identical bytes.
//
// The local implementation is kept deliberately rather than calling the shared one. A chain link is
// immutable stored data: if these hashes came from a dependency, a release of that dependency which
// changed its output by one byte would permanently invalidate every audit chain in every install,
// and a routine upgrade would be enough to do it. So the serializer that hashes history is frozen
// here, and this test is the contract that keeps it honest. If the two ever diverge, this fails and
// a person decides which one is right rather than a version bump deciding silently.
//
// The sweep covers invalid UTF-8 on purpose. A request path reaches the audit log percent-decoded,
// so arbitrary bytes are reachable without any encoding trick, and that is the input class where
// two implementations are most likely to disagree.
func TestCanonicalMatchesTheSharedSerializer(t *testing.T) {
	t.Parallel()
	fixed := []string{
		"", "plain", "a&b", "a<b>c", "x y", "x y", "quote\"here", "back\\slash",
		"tab\there", "nl\nhere", "cr\rhere", "\x00\x01\x1f", "emoji \U0001F600",
		"percent%25", "\xff\xfe invalid", "/v1/credentials/prod&staging",
		"café", "� replacement", "\x7f delete", "\u2028 line sep", "\u2029 para sep",
	}
	check := func(vals []string) {
		t.Helper()
		mine := canonicalStrings(vals)
		anyVals := make([]any, len(vals))
		for i, v := range vals {
			anyVals[i] = v
		}
		theirs, err := jcs.Serialize(anyVals)
		if err != nil {
			t.Fatalf("jcs.Serialize(%q) error = %v", vals, err)
		}
		if mine != string(theirs) {
			t.Fatalf("DIVERGENCE for %q:\n  switchtender: %s\n  loomseal jcs: %s",
				vals, mine, theirs)
		}
	}
	for _, v := range fixed {
		check([]string{v})
	}
	check(fixed)

	// Random sweep over the whole byte space, including invalid UTF-8, which is what a request path
	// can carry once it is percent-decoded.
	r := rand.New(rand.NewSource(20260801))
	for i := 0; i < 200000; i++ {
		n := r.Intn(4) + 1
		vals := make([]string, n)
		for j := range vals {
			b := make([]byte, r.Intn(12))
			for k := range b {
				b[k] = byte(r.Intn(256))
			}
			vals[j] = string(b)
		}
		check(vals)
	}
	t.Log("no divergence over 200000 random cases plus the fixed edge cases")
}

// Known-answer links for the switchtender-audit-v1 construction, shared with the LoomSeal verifier.
//
// These constants were computed from the specification by a separate implementation, not by this
// package and not by LoomSeal, and the same values are pinned on the verifier side. Both ends are
// therefore anchored to the format rather than to each other, so neither can drift without failing.
//
// This test exists because nothing here ever checked it. This package imports only LoomSeal's jcs
// and seal helpers, never its chain verifier, so when the link construction moved from a positional
// six-field array to a canonical object, every published verifier began rejecting every bundle this
// product emits and no test in either repository objected. The bug was invisible for two releases.
const (
	// katLinkBase covers the six fields every entry carries.
	katLinkBase = "4e03f42f52842aa7f4f086d13a210a6856f6a0abadea183e257d3c7554e2211c"
	// katLinkExtended adds the optional actor type, delegated account, and content digest.
	katLinkExtended = "b89efb20412f0b328d59bd2266ed09469e42218ebdc8b4a62dfc4b5dcd680efd"
	// katLinkEscapes covers a path holding the characters RFC 8785 emits raw and encoding/json
	// escapes: an ampersand, angle brackets, and U+2028.
	katLinkEscapes = "156f98eff9b037a9ea05028c6778889a2424d19136f647ec284353794a6735bc"
)

// TestEntryHashKnownAnswers pins the bytes of a chain link this product writes.
//
// A link is immutable stored history and a published verifier recomputes it, so the construction is
// a wire format shared with every relying party, not an implementation detail. Changing it silently
// invalidates every chain already written and every verifier already distributed. If this test
// fails, the fix is not to update the constants: it is to decide, deliberately, that the format is
// changing, and to release a verifier that accepts the new form before any producer emits it.
func TestEntryHashKnownAnswers(t *testing.T) {
	t.Parallel()
	at, err := time.Parse(time.RFC3339, "2026-07-27T15:00:00Z")
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	tests := []struct {
		Name  string
		Entry *Entry
		Want  string
	}{{ // Test 0: The base construction over the six fields every entry carries.
		Name: "base",
		Entry: &Entry{
			Seq: 1, At: at, Actor: "release-token", Method: "POST", Path: "/api/runs",
		},
		Want: katLinkBase,
	}, { // Test 1: The optional fields are hashed only when the entry carries them.
		Name: "extended",
		Entry: &Entry{
			Seq: 1, At: at, Actor: "release-token", Method: "POST", Path: "/api/runs",
			ActorType: "token", OnBehalfOf: "ops@example.com",
			ContentDigest: "sha256:deadbeef",
		},
		Want: katLinkExtended,
	}, { // Test 2: Characters RFC 8785 emits raw must not be escaped before hashing. The separator
		// is written as an escape rather than the literal character, which is invisible in an editor
		// and reads as an ordinary space.
		Name: "json escapes",
		Entry: &Entry{
			Seq: 1, At: at, Actor: "release-token", Method: "POST",
			Path: "/api/runs/prod&staging<x>\u2028y",
		},
		Want: katLinkEscapes,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := EntryHash(test.Entry); got != test.Want {
				t.Errorf("the chain link construction changed.\n got  %s\n want %s\n"+
					"Every published verifier recomputes this. Do not update the constant to match "+
					"the code without releasing a verifier that accepts the new form first.",
					got, test.Want)
			}
		})
	}
}
