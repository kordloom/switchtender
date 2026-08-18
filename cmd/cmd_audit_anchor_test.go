package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// seedReceiptableRun seeds a database with a chain holding one run's creation and outcome, the
// state serve leaves behind, and returns the creation entry.
func seedReceiptableRun(t *testing.T, db, runID string) *audit.Entry {
	t.Helper()
	ctx := context.Background()
	store, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	creation := &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "alice", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := store.Audits().Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	r := &run.Run{
		ID: runID, Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "alice", AuditReceipt: audit.Receipt(creation), CreatedAt: at,
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	r.Status = run.StatusSucceeded
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	if err := outcome.Commit(ctx, store.Audits(), store.Runs(), r, "system:dispatcher", nil); err != nil {
		t.Fatalf("outcome.Commit() error = %v", err)
	}
	return creation
}

// TestTreeAnchorKeepsEvidenceExportable proves a tree anchor does not poison the install's own
// evidence exports. The anchor fixes the Merkle root over the whole chain, a different coordinate
// space from the linear links, so the anchor check must recompute the tree rather than hold the
// root against the entry hash map. Before that, one `audit anchor --tree` made every bundle,
// receipt, and redemption report a healthy chain as tampered, permanently.
func TestTreeAnchorKeepsEvidenceExportable(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	creation := seedReceiptableRun(t, db, "run_anchored")

	// Fix the tree root outside the install, by reference so no network is needed.
	anchorDB, anchorType, anchorTree = db, audit.AnchorHTTPS, true
	anchorRef = "https://anchors.example/head"
	t.Cleanup(func() {
		anchorDB, anchorType, anchorRef, anchorTree = defaultDBPath, audit.AnchorRFC3161, "", false
		bundleDB, bundleOut = defaultDBPath, ""
		receiptRunDB, receiptRunOut, receiptSparse = defaultDBPath, "", false
		receiptDB = defaultDBPath
		verifyPubkey = ""
	})
	if err := runAuditAnchor(testCommand(), nil); err != nil {
		t.Fatalf("runAuditAnchor(--tree) error = %v", err)
	}

	// The full bundle must still export, and it must verify offline.
	bundlePath := filepath.Join(dir, "audit.loomseal.json")
	bundleDB, bundleOut = db, bundlePath
	if err := runAuditBundle(testCommand(), nil); err != nil {
		t.Fatalf("runAuditBundle() after a tree anchor error = %v", err)
	}
	signed, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("ReadFile(bundle) error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle(bundle) error = %v", err)
	}
	if !rep.OK() {
		t.Fatalf("the exported bundle does not verify: %+v", rep)
	}

	// The contiguous receipt must still export and verify.
	receiptPath := filepath.Join(dir, "run.receipt")
	receiptRunDB, receiptRunOut, receiptSparse = db, receiptPath, false
	if err := runReceipt(testCommand(), []string{"run_anchored"}); err != nil {
		t.Fatalf("runReceipt() after a tree anchor error = %v", err)
	}
	verifyPubkey = ""
	if err := runVerify(testCommand(), []string{receiptPath}); err != nil {
		t.Fatalf("runVerify(receipt) error = %v", err)
	}

	// The sparse receipt proves membership in exactly the coordinate the anchor fixed, so it must
	// carry the anchor and verify as anchored.
	sparsePath := filepath.Join(dir, "run.sparse.receipt")
	receiptRunOut, receiptSparse = sparsePath, true
	if err := runReceipt(testCommand(), []string{"run_anchored"}); err != nil {
		t.Fatalf("runReceipt(--sparse) after a tree anchor error = %v", err)
	}
	sparse, err := os.ReadFile(sparsePath)
	if err != nil {
		t.Fatalf("ReadFile(sparse) error = %v", err)
	}
	sparseRep, err := audit.VerifyBundle(sparse, "")
	if err != nil {
		t.Fatalf("VerifyBundle(sparse) error = %v", err)
	}
	if !sparseRep.OK() {
		t.Fatalf("the root-anchored sparse receipt does not verify: %+v", sparseRep)
	}
	if sparseRep.AnchorCount != 1 {
		t.Errorf("sparse receipt anchors = %d, want the tree anchor carried", sparseRep.AnchorCount)
	}

	// Redeeming a held receipt against the chain must still work.
	receiptDB = db
	if err := runAuditReceipt(testCommand(), []string{audit.Receipt(creation)}); err != nil {
		t.Fatalf("runAuditReceipt() after a tree anchor error = %v", err)
	}
}

// TestAnchorDeleteRecoversAPoisonedExport proves a bad anchor is recoverable. An anchor recorded
// over coordinates the chain never had fails every export, and before the delete path existed
// nothing could remove it, so one bad anchor disabled evidence export permanently.
func TestAnchorDeleteRecoversAPoisonedExport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	seedReceiptableRun(t, db, "run_poisoned")

	store, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	bad := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeLinear,
		Seq: 1, Link: "not-the-link", At: time.Now().UTC(), Ref: "https://anchors.example/head",
	}
	if err := store.Audits().(audit.AnchorStore).SaveAnchor(ctx, bad); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	_ = store.Close()

	bundleDB, bundleOut = db, filepath.Join(dir, "audit.loomseal.json")
	anchorDB = db
	t.Cleanup(func() {
		bundleDB, bundleOut, anchorDB = defaultDBPath, "", defaultDBPath
	})
	if err := runAuditBundle(testCommand(), nil); err == nil {
		t.Fatal("runAuditBundle() exported over an anchor the chain cannot satisfy")
	}

	if err := runAuditAnchorDelete(testCommand(), []string{bad.ID}); err != nil {
		t.Fatalf("runAuditAnchorDelete() error = %v", err)
	}
	if err := runAuditBundle(testCommand(), nil); err != nil {
		t.Fatalf("runAuditBundle() after deleting the bad anchor error = %v", err)
	}
	signed, err := os.ReadFile(bundleOut)
	if err != nil {
		t.Fatalf("ReadFile(bundle) error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Fatalf("the recovered bundle does not verify: %+v", rep)
	}

	// Deleting the same anchor twice reports it missing rather than pretending it was removed.
	if err := runAuditAnchorDelete(testCommand(), []string{bad.ID}); err == nil {
		t.Error("runAuditAnchorDelete() of a missing anchor reported success")
	}
}

// TestSparseReceiptWarnsWhenNoTreeAnchorCovers proves a sparse receipt is never shipped silently
// unanchored. A linear anchor fixes an entry hash, which is a coordinate a tree bundle does not
// hold, so it cannot attach; the command must say so rather than hand over a receipt whose root
// nothing outside the install fixes.
func TestSparseReceiptWarnsWhenNoTreeAnchorCovers(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	seedReceiptableRun(t, db, "run_unanchored")

	// A linear anchor exists, so the chain satisfies its anchors, but nothing fixes the tree root.
	anchorDB, anchorType, anchorTree = db, audit.AnchorHTTPS, false
	anchorRef = "https://anchors.example/head"
	t.Cleanup(func() {
		anchorDB, anchorType, anchorRef, anchorTree = defaultDBPath, audit.AnchorRFC3161, "", false
		receiptRunDB, receiptRunOut, receiptSparse = defaultDBPath, "", false
	})
	if err := runAuditAnchor(testCommand(), nil); err != nil {
		t.Fatalf("runAuditAnchor() error = %v", err)
	}

	receiptRunDB, receiptRunOut, receiptSparse = db, filepath.Join(dir, "run.receipt"), true
	var stderr bytes.Buffer
	c := testCommand()
	c.SetErr(&stderr)
	if err := runReceipt(c, []string{"run_unanchored"}); err != nil {
		t.Fatalf("runReceipt(--sparse) error = %v", err)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("No tree anchor covers this receipt")) {
		t.Errorf("a sparse receipt shipped unanchored without a warning; stderr:\n%s", stderr.String())
	}
}
