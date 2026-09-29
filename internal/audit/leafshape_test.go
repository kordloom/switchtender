package audit_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestBundleClaimKeySetIsPinned guards the one assumption the Merkle leaf rests on.
//
// A leaf is hashed over an ALLOWLIST, {type, at, payload}, while the format and the reference
// verifier hash every member of the claim EXCEPT the two that describe where it sits, chain and
// inclusion. The two rules agree today only because the claim happens to have exactly those five
// members. Add a sixth and they part company silently: this producer keeps hashing three members
// while every conforming verifier hashes four, so every sparse receipt stops folding, and the
// failure appears at whoever is auditing rather than here.
//
// Pinning the key set rather than switching the leaf to a denylist is deliberate. A denylist would
// be the tidier rule, but changing what the leaf covers changes the preimage of every leaf already
// anchored, which would quietly invalidate the anchors that exist. So the rule stays and the
// assumption behind it is made to fail loudly instead.
func TestBundleClaimKeySetIsPinned(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(audit.BundleClaim{
		Type:      "switchtender.audit/1",
		At:        "2026-08-11T16:00:00Z",
		Payload:   map[string]any{"actor": "root"},
		Chain:     audit.BundleCoordLink{Seq: 1, Link: "abc", Prev: ""},
		Inclusion: &audit.BundleInclusion{Path: []string{"def"}},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	want := []string{"at", "chain", "inclusion", "payload", "type"}
	if diff := cmp.Diff(want, keys, cmpopts.SortSlices(func(a, b string) bool { return a < b }),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the claim's members changed (-want +got):\n%s\n\n"+
			"A leaf is hashed over {type, at, payload} and the format hashes everything except "+
			"chain and inclusion, so those two rules only agree while this set is exactly these "+
			"five. Adding a member here means treeLeaf must be revisited, and note that widening "+
			"what the leaf covers changes the preimage of every leaf already anchored.", diff)
	}
}
