package audit_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// buildChain returns a valid chain of n entries linked with audit.Link.
func buildChain(n int) []*audit.Entry {
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	var out []*audit.Entry
	var prev *audit.Entry
	for i := 0; i < n; i++ {
		e := &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Second),
			Actor: "root", Method: "POST", Path: "/runs",
		}
		audit.Link(prev, e)
		out = append(out, e)
		prev = e
	}
	return out
}

// TestLinkGenesis confirms the first entry starts the chain at sequence one with no predecessor.
func TestLinkGenesis(t *testing.T) {
	t.Parallel()
	e := &audit.Entry{ID: "aud_1", At: time.Unix(0, 0).UTC(), Actor: "root", Method: "POST", Path: "/runs"}
	audit.Link(nil, e)
	if e.Seq != 1 {
		t.Errorf("Seq = %d, want 1", e.Seq)
	}
	if e.PrevHash != "" {
		t.Errorf("PrevHash = %q, want empty", e.PrevHash)
	}
	if e.Hash != audit.EntryHash(e) {
		t.Errorf("Hash = %q, want the entry hash %q", e.Hash, audit.EntryHash(e))
	}
}

// TestEntryHashDependsOnContent confirms the hash changes when a field changes.
func TestEntryHashDependsOnContent(t *testing.T) {
	t.Parallel()
	e := &audit.Entry{ID: "aud_1", At: time.Unix(0, 0).UTC(), Actor: "root", Method: "POST", Path: "/runs", Seq: 1}
	before := audit.EntryHash(e)
	e.Actor = "mallory"
	if after := audit.EntryHash(e); after == before {
		t.Errorf("hash unchanged after editing actor: %q", after)
	}
}

// TestVerify covers an intact chain and the tamper cases: an edited field, a rewritten hash, a
// broken link, a deleted entry, and a blanked hash.
func TestVerify(t *testing.T) {
	t.Parallel()

	// Test 0: An intact chain verifies.
	if ok, at := audit.Verify(buildChain(3)); !ok {
		t.Errorf("intact chain: Verify broke at %d", at)
	}

	// Test 1: Editing a field without rehashing is caught at that entry.
	tampered := buildChain(3)
	tampered[1].Actor = "mallory"
	if ok, at := audit.Verify(tampered); ok || at != 2 {
		t.Errorf("edited field: ok=%v at=%d, want false at 2", ok, at)
	}

	// Test 2: Rewriting an entry's own hash is caught at that entry.
	rehashed := buildChain(3)
	rehashed[0].Hash = "deadbeef"
	if ok, at := audit.Verify(rehashed); ok || at != 1 {
		t.Errorf("rewritten hash: ok=%v at=%d, want false at 1", ok, at)
	}

	// Test 3: Breaking the link to the previous entry is caught.
	relinked := buildChain(3)
	relinked[2].PrevHash = "deadbeef"
	if ok, at := audit.Verify(relinked); ok || at != 3 {
		t.Errorf("broken link: ok=%v at=%d, want false at 3", ok, at)
	}

	// Test 4: Deleting a middle entry leaves a sequence gap that is caught.
	full := buildChain(3)
	gapped := []*audit.Entry{full[0], full[2]}
	if ok, at := audit.Verify(gapped); ok || at != 2 {
		t.Errorf("deleted entry: ok=%v at=%d, want false at 2", ok, at)
	}

	// Test 5: An entry with no hash breaks the chain, so a blanked entry cannot hide from verification.
	blanked := buildChain(3)
	blanked[1].Hash = ""
	if ok, at := audit.Verify(blanked); ok || at != 2 {
		t.Errorf("blanked hash: ok=%v at=%d, want false at 2", ok, at)
	}
}

// TestEntryHashMatchesTheSpec pins the exact link digest for entries whose fields contain the
// characters where Go's encoding/json disagrees with canonical JSON. The values below were computed
// independently by the LoomSeal reference verifier's own serializer, so they are conformance vectors
// rather than a recording of whatever this code happens to do.
//
// encoding/json escapes <, >, and & for HTML safety and U+2028 and U+2029 for JavaScript safety.
// None of those escapes belong in canonical JSON, so an audit path containing one used to hash to a
// value no independent verifier could reproduce. A request path is percent-decoded before it is
// recorded, and & is legal in a path segment, so this was reachable with no encoding trick at all.
func TestEntryHashMatchesTheSpec(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		Name string
		Path string
		Want string
	}{{ // Test 0: A plain path, where every serializer already agreed.
		Name: "plain", Path: "/v1/projects/plain",
		Want: "651aae892f9e83147a108bdcc46e9184ea3fb27f76bc303a556f7b3c1bd7273a",
	}, { // Test 1: An ampersand, which encoding/json escapes as &.
		Name: "ampersand", Path: "/v1/projects/a&b",
		Want: "aa85832c8f817a00220513e42a69d06862148ba1d4eb5d17efe4410802164d3b",
	}, { // Test 2: Angle brackets, which encoding/json escapes as < and >.
		Name: "angle brackets", Path: "/v1/p/<x>",
		Want: "488c21c89f47f2ce82e3883502d0630c62a61f803de8f54a2890f16c4e892fb0",
	}, { // Test 3: U+2028, which encoding/json escapes even with HTML escaping disabled.
		Name: "line separator", Path: "/v1/p/a b",
		Want: "9cc1e182b8342e31a9cd6027a35470347d33a0a2f7db7830438d3ae9fdd279cf",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			e := &audit.Entry{Seq: 1, At: at, Actor: "admin", Method: "POST", Path: test.Path}
			if got := audit.EntryHash(e); got != test.Want {
				t.Errorf("EntryHash() = %s, want %s\nan independent verifier will reject this link",
					got, test.Want)
			}
		})
	}
}
