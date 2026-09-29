package audit

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
	"unicode/utf8"
)

// TestEscapingMakesEveryEntryHashable pins the property the producing side depends on: after
// escaping, no text a request can carry stops a chain link from being computed.
//
// This replaces a test that compared a local copy of the canonical serializer against LoomSeal's.
// That comparison had stopped meaning anything. The local copy went dead when the link construction
// moved from a positional array to a canonical object, so nothing hashed with it, while the claim
// in its comment was that it protected stored hashes from a dependency bump. What actually protects
// them is TestEntryHashKnownAnswers below, which pins output bytes computed from the specification.
// The comparison then broke outright when LoomSeal began refusing invalid UTF-8 rather than
// silently mapping it to U+FFFD, which is the better behavior and is why the escaping exists here.
//
// So the sweep is kept and pointed at the real invariant. Serializing now fails on invalid UTF-8,
// and the producing side panics on a link it cannot compute, which means any unescaped byte
// reaching EntryHash is a crash. The escaping is what makes that unreachable.
func TestEscapingMakesEveryEntryHashable(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	check := func(raw string) {
		t.Helper()
		escaped := escapeInvalidUTF8(raw)
		if !utf8.ValidString(escaped) {
			t.Fatalf("escaping %q left invalid UTF-8: %q", raw, escaped)
		}
		// Through Link, which is the only way an entry is built, so the test cannot pass by
		// escaping something the producer does not escape.
		e := &Entry{Seq: 1, At: at, Actor: raw, Method: "GET", Path: raw, ActorType: raw,
			OnBehalfOf: raw}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("hashing an entry holding %q panicked: %v", raw, r)
			}
		}()
		Link(nil, e)
		if e.Hash == "" {
			t.Fatalf("no link computed for %q", raw)
		}
	}
	for _, v := range hostileStrings {
		check(v)
	}

	// Random sweep over the whole byte space, which is what a request path can carry once it is
	// percent-decoded.
	r := rand.New(rand.NewSource(20260801))
	for i := 0; i < 200000; i++ {
		b := make([]byte, r.Intn(12))
		for k := range b {
			b[k] = byte(r.Intn(256))
		}
		check(string(b))
	}
}

// TestEscapingCannotCollideTwoDistinctInputs pins that the escaping is injective.
//
// This is the security property, not a nicety. Ranging over a string decodes it, so every invalid
// byte arrives as U+FFFD and, without escaping, DELETE /v1/credentials/\xff and the same path
// ending \xfe hash to one link and become indistinguishable in the audit record. Percent is
// rewritten too, or the escaping reintroduces the collision one level up between the three
// characters %FF and the single byte 0xFF, and both are reachable from a request.
func TestEscapingCannotCollideTwoDistinctInputs(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string)
	check := func(raw string) {
		t.Helper()
		escaped := escapeInvalidUTF8(raw)
		if prior, ok := seen[escaped]; ok && prior != raw {
			t.Fatalf("COLLISION: %q and %q both escape to %q", prior, raw, escaped)
		}
		seen[escaped] = raw
	}
	for _, v := range hostileStrings {
		check(v)
	}
	// The pairs the escaping exists to separate, stated outright rather than left to the sweep.
	for _, pair := range [][2]string{
		{"\xff", "\xfe"},
		{"%FF", "\xff"},
		{"%25", "%"},
		{"/v1/credentials/\xff", "/v1/credentials/\xfe"},
	} {
		if a, b := escapeInvalidUTF8(pair[0]), escapeInvalidUTF8(pair[1]); a == b {
			t.Errorf("%q and %q both escape to %q", pair[0], pair[1], a)
		}
	}

	r := rand.New(rand.NewSource(20260802))
	for i := 0; i < 200000; i++ {
		b := make([]byte, r.Intn(8))
		for k := range b {
			b[k] = byte(r.Intn(256))
		}
		check(string(b))
	}
}

// hostileStrings are the fixed edge cases both sweeps cover: the characters RFC 8785 emits raw and
// encoding/json escapes, the control range, invalid UTF-8, and percent in both its forms.
var hostileStrings = []string{
	"", "plain", "a&b", "a<b>c", "x y", "quote\"here", "back\\slash",
	"tab\there", "nl\nhere", "cr\rhere", "\x00\x01\x1f", "emoji \U0001F600",
	"percent%25", "percent%", "\xff\xfe invalid", "/v1/credentials/prod&staging",
	"caf\u00e9", "\ufffd replacement", "\x7f delete", "\u2028 line sep", "\u2029 para sep",
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
	// katLinkInstall covers the install id, which binds a link to the install that wrote it so a
	// receipt cannot be lifted onto another one. It is the newest field in the construction, so it
	// is the one most likely to be got wrong by a verifier written against an older description.
	katLinkInstall = "cf038af2b15d83a89388eda89fc71342911b55a817dd39e57021badfd21f4b13"
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
	}, { // Test 3: The install id participates in the link, and sorts between at and method.
		Name: "install id",
		Entry: &Entry{
			Seq: 1, At: at, Actor: "release-token", Method: "POST", Path: "/api/runs",
			InstallID: "in_0123456789ab",
		},
		Want: katLinkInstall,
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
