package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/identity"
)

// upgradedInstall returns an identity whose install id is the widened derivation, along with the
// name the same key was known by before the derivation changed. It fails the test if the two agree,
// since then nothing here is being told apart.
func upgradedInstall(t *testing.T) (Identity, string) {
	t.Helper()
	t.Setenv(identity.KeyEnv, "")
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	narrow := identity.LegacyInstallIDFromKey(id.Public())
	if narrow == "" || narrow == id.InstallID {
		t.Fatalf("the two install id derivations produced %q and %q, so this test cannot tell the "+
			"widening apart", narrow, id.InstallID)
	}
	if id.InstallID != identity.InstallIDFromKey(id.Public()) {
		t.Fatalf("a freshly created identity is named %q rather than its key's derivation, so this "+
			"test is not set up the way an install that re-derives its id is", id.InstallID)
	}
	return id, narrow
}

// TestAChainWrittenAcrossTheIdWideningStillVerifies covers what an operator sees the first time they
// upgrade past the change that widened the install id.
//
// An install id is derived from the producer key on first boot and stored, so most installs keep the
// name they already had. One shape re-derives it instead: SWITCHTENDER_AUDIT_KEY supplies the key
// directly and that path keeps no identity file, which is the documented way to run a shared
// postgres chain. Upgrading renamed those installs underneath a chain already written, so entries
// before the upgrade name one id and entries after it name another.
//
// Each entry's link commits to the id it carried, and a link already written commits to what it
// committed to. Demanding byte equality with the producer therefore reported the seam as a broken
// chain: not a warning, the same verdict a rewritten entry gets. The fix that shipped first repaired
// the producer block alone and left every entry beneath it failing, which is the half a header-only
// test cannot see.
func TestAChainWrittenAcrossTheIdWideningStillVerifies(t *testing.T) {
	id, narrow := upgradedInstall(t)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	// Two entries under the name in force before the upgrade, then two under the name after it.
	names := []string{narrow, narrow, id.InstallID, id.InstallID}
	entries := make([]*Entry, 0, len(names))
	var prev *Entry
	for i, name := range names {
		e := &Entry{
			ID: "aud_width_" + string(rune('a'+i)), At: at.Add(time.Duration(i) * time.Second),
			Actor: "release-token", ActorType: "token", Method: "POST", Path: "/v1/runs",
			InstallID: name,
		}
		Link(prev, e)
		entries = append(entries, e)
		prev = e
	}
	if ok, brokeAt := Verify(entries); !ok {
		t.Fatalf("the fixture chain does not verify at %d before any of this is asked", brokeAt)
	}

	doc, err := BuildBundle(entries, id, "1.99.0", at)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("a chain spanning the upgrade does not verify: %v", err)
	}
	if !rep.ChainOK {
		t.Errorf("the chain reads as broken at sequence %d, which is the verdict a rewritten entry "+
			"gets. The entries before the upgrade name this install by the width it used then, and "+
			"nothing can rewrite a link already written", rep.BrokeAtSeq)
	}

	// An entry naming an install this key was never known by, at either width, is still refused.
	// The grandfathering is one key's two names, not any name at all.
	lifted := make([]*Entry, len(entries))
	for i, e := range entries {
		cp := *e
		lifted[i] = &cp
	}
	lifted[1].InstallID = "in_ffffffffffff"
	var relinked *Entry
	for _, e := range lifted {
		Link(relinked, e)
		relinked = e
	}
	doc2, err := BuildBundle(lifted, id, "1.99.0", at)
	if err != nil {
		t.Fatalf("BuildBundle() for the lifted chain error = %v", err)
	}
	signed2, err := SignBundleDoc(doc2, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() for the lifted chain error = %v", err)
	}
	rep2, err := VerifyBundle(signed2, id.KeyID())
	if err == nil && rep2.ChainOK {
		t.Error("an entry naming an install this key was never known by verified, so the " +
			"grandfathering accepts any name rather than this key's own earlier one")
	}
}

// TestATreeAnchorTakenBeforeTheIdWideningStillRecomputes covers the same upgrade one layer out.
//
// A tree anchor fixes a Merkle root whose leaves commit to the install id, so the same untouched
// chain under a different name produces a different root. When the upgrade renamed an install, every
// anchor it had already taken stopped recomputing, and the verdict was that the tree could not be
// recomputed here and the operator should restore the producer key that made the chain. They had it.
// Worse than the wording: an unreached anchor refuses to sign, so those installs could not export a
// receipt at all.
//
// Only this key's own earlier name is folded. An anchor naming some other install is still reported
// as unrecomputable, because a process that cannot reproduce an identity must not vouch for a root
// computed under it.
func TestATreeAnchorTakenBeforeTheIdWideningStillRecomputes(t *testing.T) {
	id, narrow := upgradedInstall(t)
	entries := buildChain(t, 4)

	size, root, err := TreeHead(entries, narrow)
	if err != nil {
		t.Fatalf("TreeHead() under the earlier name error = %v", err)
	}
	anchor := &Anchor{
		ID: "anc_width", Type: AnchorHTTPS, Shape: AnchorShapeTree, Seq: size, Link: root,
		At: time.Now(), Ref: "https://example.com/head", InstallID: narrow,
	}

	ok, results := CheckAnchors(entries, []*Anchor{anchor}, id)
	if !ok {
		t.Errorf("an anchor this install took under its own earlier name no longer recomputes, so "+
			"the install cannot export a receipt at all: %s", results[0].Problem)
	}

	// The same anchor under a name that is not this key's is still refused, and still says so
	// without calling it a rewrite.
	foreign := &Anchor{
		ID: "anc_foreign", Type: AnchorHTTPS, Shape: AnchorShapeTree, Seq: size, Link: root,
		At: time.Now(), Ref: "https://example.com/head", InstallID: "in_0123456789ab",
	}
	ok, results = CheckAnchors(entries, []*Anchor{foreign}, id)
	if ok {
		t.Error("an anchor naming an unrelated install recomputed, so the fold now trusts whatever " +
			"name an anchor carries")
	}
	if results[0].Reached {
		t.Errorf("verdict = %+v, want the anchor unreached", results[0])
	}
}

// TestARewriteUnderTheEarlierNameIsStillReportedAsARewrite covers the one message that must never
// be softened.
//
// An anchor taken under this install's earlier name is recomputed under that name, so an upgraded
// install can still check its own anchors. That repair introduced a way to say the wrong thing: when
// the alternate fold also disagreed, the verdict fell through to the identity-mismatch wording,
// which ends in "Nothing here says the history changed". It did. The process had reproduced the
// exact identity the anchor names and the roots still differed, which is proof of a rewrite, and the
// proof was discarded in favor of a sentence denying it.
//
// An auditor reading that about a chain somebody had actually edited would close the question.
func TestARewriteUnderTheEarlierNameIsStillReportedAsARewrite(t *testing.T) {
	id, narrow := upgradedInstall(t)
	entries := buildChain(t, 6)

	size, root, err := TreeHead(entries, narrow)
	if err != nil {
		t.Fatalf("TreeHead() under the earlier name error = %v", err)
	}
	anchor := &Anchor{
		ID: "anc_pre_upgrade", Type: AnchorHTTPS, Shape: AnchorShapeTree, Seq: size, Link: root,
		At: time.Now(), Ref: "https://example.com/head", InstallID: narrow,
	}

	// Untouched, the anchor is satisfied under the name it was taken with.
	if ok, results := CheckAnchors(entries, []*Anchor{anchor}, id); !ok {
		t.Fatalf("the anchor does not verify over the chain it was taken on: %s", results[0].Problem)
	}

	// Now somebody edits an entry and relinks, so the hash chain still verifies on its own terms.
	// The anchor is the only thing that can catch it, and it must say so.
	tampered := make([]*Entry, len(entries))
	for i, e := range entries {
		cp := *e
		tampered[i] = &cp
	}
	tampered[1].Actor = "mallory"
	var prev *Entry
	for _, e := range tampered {
		e.PrevHash = ""
		if prev != nil {
			e.PrevHash = prev.Hash
		}
		e.Hash = EntryHash(e)
		prev = e
	}
	if ok, at := Verify(tampered); !ok {
		t.Fatalf("the relinked chain does not self-verify at %d, so the anchor is not the only "+
			"thing that would catch this and the test proves less than it claims", at)
	}

	ok, results := CheckAnchors(tampered, []*Anchor{anchor}, id)
	if ok {
		t.Fatal("an edited chain satisfied the anchor taken over it")
	}
	problem := results[0].Problem
	if !strings.Contains(problem, "rewritten") {
		t.Errorf("verdict = %q, want it to say the history under the anchor was rewritten. The "+
			"process reproduced the identity this anchor names and the roots still differ, which "+
			"is the proof, and any other wording discards it", problem)
	}
	if strings.Contains(problem, "Nothing here says the history changed") {
		t.Errorf("verdict = %q, which denies the very thing it just proved", problem)
	}
}

// TestATreeAnchorWithNoInstallIDStillRecomputes covers the anchors written before anchors recorded
// which install took them.
//
// Those came from the same releases that carried the narrow id derivation, so the narrow name is
// what their roots were computed under. Everything else in this package grandfathers a pre-binding
// record rather than reading it as a fault; leaving this one shape out of the alias fold made it the
// single case that reports an untouched chain as rewritten, and an unreached anchor refuses to sign
// a bundle, so the install could not export a receipt at all.
func TestATreeAnchorWithNoInstallIDStillRecomputes(t *testing.T) {
	id, narrow := upgradedInstall(t)
	entries := buildChain(t, 4)

	size, root, err := TreeHead(entries, narrow)
	if err != nil {
		t.Fatalf("TreeHead() under the earlier name error = %v", err)
	}
	anchor := &Anchor{
		ID: "anc_unnamed", Type: AnchorHTTPS, Shape: AnchorShapeTree, Seq: size, Link: root,
		At: time.Now(), Ref: "https://example.com/head",
	}

	ok, results := CheckAnchors(entries, []*Anchor{anchor}, id)
	if !ok {
		t.Errorf("an anchor written before anchors recorded an install reports an untouched chain "+
			"as unreached, so this install cannot export a receipt: %s", results[0].Problem)
	}
}
