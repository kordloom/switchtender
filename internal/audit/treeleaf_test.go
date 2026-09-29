package audit

import (
	"encoding/json"
	"testing"
)

// TestTreeLeafCommitsToMembersThisBuildDoesNotModel covers a claim carrying a member the struct does
// not declare.
//
// A leaf is defined by removing a fixed set of members, not by listing the ones this version knows.
// Building it from the decoded struct instead silently dropped everything else the claim carried, so
// a claim holding an evidence entry hashed to one leaf here and a different one at the reference,
// and its receipt stopped verifying. The struct models no evidence member, which is exactly why this
// has to be checked against the claim's own bytes.
func TestTreeLeafCommitsToMembersThisBuildDoesNotModel(t *testing.T) {
	t.Parallel()
	const install = "install-abc"
	base := `{"type":"switchtender.audit/1","at":"2026-01-01T00:00:00Z",` +
		`"payload":{"actor":"a","method":"POST","path":"/x"}`

	var bare, withEvidence BundleClaim
	if err := json.Unmarshal([]byte(base+`}`), &bare); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal([]byte(base+`,"evidence":[{"role":"snapshot",`+
		`"digest":"sha256:aa","media_type":"text/html"}]}`), &withEvidence); err != nil {
		t.Fatalf("decode: %v", err)
	}

	bareLeaf, err := treeLeafFor(bare, install)
	if err != nil {
		t.Fatalf("treeLeafFor: %v", err)
	}
	evLeaf, err := treeLeafFor(withEvidence, install)
	if err != nil {
		t.Fatalf("treeLeafFor: %v", err)
	}
	if string(bareLeaf) == string(evLeaf) {
		t.Fatal("a claim carrying evidence produced the same leaf as one without it, so the " +
			"evidence is outside the commitment and could be swapped without breaking the chain")
	}
}

// TestTreeLeafIgnoresEvidencePackaging is the other half of the same rule.
//
// present and location say whether and where an artifact travels beside this particular copy. If
// they entered the commitment, the same entry disclosed once with its transcript attached and once
// without would hash differently, and one of the two could never fold to the anchored root.
func TestTreeLeafIgnoresEvidencePackaging(t *testing.T) {
	t.Parallel()
	const install = "install-abc"
	base := `{"type":"switchtender.audit/1","at":"2026-01-01T00:00:00Z",` +
		`"payload":{"actor":"a","method":"POST","path":"/x"},"evidence":[{"role":"snapshot",` +
		`"digest":"sha256:aa","media_type":"text/html"%s}]}`

	var packed, unpacked BundleClaim
	if err := json.Unmarshal([]byte(jsonf(base, `,"present":true,"location":"evidence/s.html"`)),
		&packed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal([]byte(jsonf(base, "")), &unpacked); err != nil {
		t.Fatalf("decode: %v", err)
	}

	a, err := treeLeafFor(packed, install)
	if err != nil {
		t.Fatalf("treeLeafFor: %v", err)
	}
	b, err := treeLeafFor(unpacked, install)
	if err != nil {
		t.Fatalf("treeLeafFor: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("packaging changed the leaf, so the same entry packaged two ways cannot both fold " +
			"to the root the producer anchored")
	}
}

// jsonf fills the packaging slot in the fixtures above.
func jsonf(format, packaging string) string {
	out := make([]byte, 0, len(format)+len(packaging))
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) && format[i+1] == 's' {
			out = append(out, packaging...)
			i++
			continue
		}
		out = append(out, format[i])
	}
	return string(out)
}
