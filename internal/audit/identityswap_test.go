package audit

import (
	"strings"
	"testing"
	"time"
)

// TestATreeAnchorNamesTheInstallItWasTakenUnder covers the most misleading message the product can
// produce. A tree anchor fixes a Merkle root whose leaves are bound to the install's identity, so the
// same chain under a different identity recomputes a different root. Two ordinary events produce that:
// a database restored onto a host whose key file did not come with it, and a PostgreSQL deployment where
// each replica mints its own key because the key lives in a file beside a database that is not there.
//
// The verdict was "the history under the anchor was rewritten". Nothing had been rewritten. An operator
// reading that about their own audit trail either doubts a chain that is fine or, worse, learns to
// dismiss the one message that says their history was altered. The anchor now records which install took
// it, so the mismatch is diagnosed instead of guessed at.
func TestATreeAnchorNamesTheInstallItWasTakenUnder(t *testing.T) {
	t.Parallel()
	entries := buildChain(t, 4)
	const madeBy = "inst_original"
	const nowRunning = "inst_restored"

	size, root, err := TreeHead(entries, madeBy)
	if err != nil {
		t.Fatalf("TreeHead: %v", err)
	}
	anchor := &Anchor{
		ID: "anc_1", Type: AnchorHTTPS, Shape: AnchorShapeTree, Seq: size, Link: root,
		At: time.Now(), Ref: "https://example.com/head", InstallID: madeBy,
	}

	// Test 0: Checked under the identity that took it, the anchor is satisfied.
	ok, results := CheckAnchors(entries, []*Anchor{anchor}, madeBy)
	if !ok {
		t.Fatalf("the anchor does not verify under its own install: %+v", results)
	}

	// Test 1: Checked under a different identity, the same anchor is refused, and the refusal says why
	// rather than claiming the history changed.
	ok, results = CheckAnchors(entries, []*Anchor{anchor}, nowRunning)
	if ok {
		t.Fatal("the anchor verified under a different install identity, so the identity is not " +
			"actually bound into the tree")
	}
	problem := results[0].Problem
	if strings.Contains(problem, "rewritten") {
		t.Errorf("verdict = %q, want it to name the identity mismatch rather than claim the history "+
			"was rewritten", problem)
	}
	for _, want := range []string{madeBy, nowRunning, "producer key"} {
		if !strings.Contains(problem, want) {
			t.Errorf("verdict = %q, want it to mention %q", problem, want)
		}
	}

	// Test 2: An anchor from before install ids were recorded still reports the old way, since nothing
	// can tell the two causes apart for it.
	older := &Anchor{
		ID: "anc_0", Type: AnchorHTTPS, Shape: AnchorShapeTree, Seq: size, Link: root,
		At: time.Now(), Ref: "https://example.com/head",
	}
	_, results = CheckAnchors(entries, []*Anchor{older}, nowRunning)
	if !strings.Contains(results[0].Problem, "rewritten") {
		t.Errorf("verdict for an anchor with no recorded install = %q, want the original wording",
			results[0].Problem)
	}
}

// TestASharedDatabaseWillNotMintAPerHostIdentity covers the deployment the identity model quietly does
// not fit. The signing identity lives in a file beside the database, which is right for SQLite, where the
// database is a file in the same directory. Against PostgreSQL there is no such directory: the key was
// created under the operating system user's config directory, so every replica in an active-active pair,
// and every host running an anchor job over the one shared chain, minted its own identity and signed as a
// different install. Bundles from two replicas were attributable to two installs, and a tree anchor taken
// by one could not be recomputed by the other, which reads as a rewritten chain.
//
// A shared chain needs one identity, and there is no way for a process to invent it: it has to be
// configured. So a Postgres deployment with no identity supplied is refused, with the command that makes
// one, rather than starting with a key nobody else has.
func TestASharedDatabaseWillNotMintAPerHostIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")

	// Test 0: A shared store with no identity anywhere is refused, and the refusal says what to do.
	_, err := LoadIdentityForStore("postgres://user@db.example.com/switchtender", dir)
	if err == nil {
		t.Fatal("a Postgres deployment minted its own identity, so each replica signs as a different " +
			"install and a tree anchor from one cannot be checked by another")
	}
	for _, want := range []string{"SWITCHTENDER_AUDIT_KEY", "every"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}

	// Test 1: With a key supplied, it is used, and every replica given the same key agrees.
	const seed = "5b2f8c1d4e6a7b9c0d1e2f3a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e"
	t.Setenv("SWITCHTENDER_AUDIT_KEY", seed)
	shared, err := LoadIdentityForStore("postgres://user@db.example.com/switchtender", dir)
	if err != nil {
		t.Fatalf("LoadIdentityForStore with a supplied key = %v", err)
	}
	other, err := LoadIdentityForStore("postgres://user@db.example.com/switchtender", t.TempDir())
	if err != nil {
		t.Fatalf("second replica = %v", err)
	}
	if shared.InstallID != other.InstallID || shared.KeyID() != other.KeyID() {
		t.Errorf("two replicas given the same key resolved to %s/%s and %s/%s, want one identity",
			shared.InstallID, shared.KeyID(), other.InstallID, other.KeyID())
	}

	// Test 2: A key file already in the identity directory is honored too, since that is how an operator
	// distributes one without putting it in the environment.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	fileDir := t.TempDir()
	created, err := LoadIdentityForStore("switchtender.db", fileDir)
	if err != nil {
		t.Fatalf("SQLite deployment = %v", err)
	}
	reread, err := LoadIdentityForStore("postgres://user@db.example.com/switchtender", fileDir)
	if err != nil {
		t.Fatalf("Postgres with a key file present = %v", err)
	}
	if reread.KeyID() != created.KeyID() {
		t.Errorf("the key file was ignored: %s, want %s", reread.KeyID(), created.KeyID())
	}
}
