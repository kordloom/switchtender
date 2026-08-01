package audit

import (
	"math/rand"
	"testing"

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
