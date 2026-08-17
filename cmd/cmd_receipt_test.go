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

// TestReceiptProduceAndVerifyOffline is the end-to-end proof of the receipt wedge: a finished run is
// turned into a signed receipt, and that receipt verifies with no database and no network. Then the
// receipt is altered and fails, so a receipt cannot be doctored and still pass.
func TestReceiptProduceAndVerifyOffline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	out := filepath.Join(dir, "run.receipt")

	// Seed a run whose creation and outcome are on the chain, the state serve leaves behind.
	store, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	// Deliberately not UTC: a server's clock carries a local offset, the store writes every
	// timestamp as UTC, and a receipt rebuilds the outcome from the stored run. Building this
	// fixture in UTC was why the suite agreed the receipt verified while it failed on every
	// install west or east of Greenwich.
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.FixedZone("CST", -6*60*60))
	creation := &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "alice", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := store.Audits().Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	// Mirror the real flow: evidence is written while the run is running, then it is finalized, then
	// its outcome is committed. Log append is fenced to a non-terminal run, so the order matters.
	r := &run.Run{
		ID: "run_demo", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "alice", ActorType: "agent", AuditReceipt: audit.Receipt(creation), CreatedAt: at,
		ExtraVars: map[string]any{"service": "api"},
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	// The approval decision lands on the chain between creation and outcome, binding the approver
	// to the spec digest, exactly as the dispatcher commits it.
	if _, err := outcome.CommitDecision(ctx, store.Audits(), r, "approved",
		"approver-pat", "session"); err != nil {
		t.Fatalf("CommitDecision() error = %v", err)
	}
	if err := store.Runs().AppendLog(ctx, "run_demo", []byte("PLAY RECAP\nweb01 : ok=5 changed=1\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := store.Runs().SaveHostSummary(ctx, "run_demo",
		[]run.HostSummary{{Host: "web01", OK: 5, Changed: 1, Worst: "changed"}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	// A fractional task duration is what every real run records, and it once made the range
	// receipt unsignable under the integer-only JCS profile.
	if err := store.Runs().SaveTaskSummary(ctx, "run_demo",
		[]run.TaskSummary{{Task: "say hello", Seconds: 0.013500213}}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}
	r.Status = run.StatusSucceeded
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	if err := outcome.Commit(ctx, store.Audits(), store.Runs(), r, "system:dispatcher"); err != nil {
		t.Fatalf("outcome.Commit() error = %v", err)
	}
	_ = store.Close()

	// Produce the receipt.
	receiptRunDB, receiptRunOut = db, out
	t.Cleanup(func() { receiptRunDB, receiptRunOut, verifyPubkey = defaultDBPath, "", "" })
	if err := runReceipt(testCommand(), []string{"run_demo"}); err != nil {
		t.Fatalf("runReceipt() error = %v", err)
	}

	// Verify it offline. runVerify returns nil only when every check passes.
	verifyPubkey = ""
	var buf bytes.Buffer
	c := testCommand()
	c.SetOut(&buf)
	if err := runVerify(c, []string{out}); err != nil {
		t.Fatalf("runVerify() of a genuine receipt error = %v\n%s", err, buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("VERIFIED")) {
		t.Errorf("verify output missing VERIFIED:\n%s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("run run_demo")) {
		t.Errorf("verify output does not name the run subject:\n%s", buf.String())
	}
	// The receipt discloses the outcome and verify shows what the run did, matching the commitment.
	for _, want := range []string{"outcome      OK", "what happened", "web01", "ok=5 changed=1"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("verify output missing %q:\n%s", want, buf.String())
		}
	}
	// It also shows who decided and proves the approved, executed, and disclosed spec digests are
	// the same change.
	for _, want := range []string{"decisions    OK", "approved by approver-pat (session)", "spec         OK"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("verify output missing %q:\n%s", want, buf.String())
		}
	}
	t.Logf("switchtender verify run.receipt:\n%s", buf.String())

	// A spec edited after the decision produces a receipt whose rebuilt decision body no longer
	// matches the digest the chain committed, and verify refuses it. This is the exact tamper the
	// binding exists to surface.
	tampered, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle(tamper) error = %v", err)
	}
	r.ExtraVars = map[string]any{"service": "api", "limit_override": "all"}
	r.Status = run.StatusSucceeded
	if err := tampered.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(tampered) error = %v", err)
	}
	_ = tampered.Close()
	tamperedOut := filepath.Join(dir, "tampered.receipt")
	receiptRunDB, receiptRunOut = db, tamperedOut
	if err := runReceipt(testCommand(), []string{"run_demo"}); err != nil {
		t.Fatalf("runReceipt(tampered) error = %v", err)
	}
	if err := runVerify(testCommand(), []string{tamperedOut}); err == nil {
		t.Error("runVerify() accepted a receipt whose spec changed after the decision")
	}

	// A doctored receipt must fail.
	signed, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	doctored := bytes.Replace(signed, []byte("/runs"), []byte("/evil"), 1)
	if bytes.Equal(doctored, signed) {
		t.Fatal("test did not alter the receipt")
	}
	if err := os.WriteFile(out, doctored, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := runVerify(testCommand(), []string{out}); err == nil {
		t.Error("runVerify() accepted a doctored receipt")
	}
}

// TestSparseReceiptDisclosesOnlyTheRun proves the wedge end to end through the commands a user runs:
// a receipt for one run, on an install whose chain also holds other runs' entries, carries this run's
// entries and nothing about the others, and still verifies.
func TestSparseReceiptDisclosesOnlyTheRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	out := filepath.Join(dir, "run.receipt")

	store, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	// Another tenant's work, recorded before and after ours, is what a contiguous receipt would sweep
	// up and hand to whoever reads it.
	neighbors := []string{"/v1/runs/other_alpha/approve", "/v1/projects/secret-migration"}
	if err := store.Audits().Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "bob", Method: "POST", Path: neighbors[0],
	}); err != nil {
		t.Fatalf("Append(neighbor) error = %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: at.Add(time.Minute), Actor: "alice", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := store.Audits().Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	if err := store.Audits().Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: at.Add(2 * time.Minute), Actor: "bob", Method: "POST",
		Path: neighbors[1],
	}); err != nil {
		t.Fatalf("Append(neighbor 2) error = %v", err)
	}

	r := &run.Run{
		ID: "run_sparse", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "alice", AuditReceipt: audit.Receipt(creation), CreatedAt: at,
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	r.Status = run.StatusSucceeded
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	if err := outcome.Commit(ctx, store.Audits(), store.Runs(), r, "system:dispatcher"); err != nil {
		t.Fatalf("outcome.Commit() error = %v", err)
	}
	_ = store.Close()

	receiptRunDB, receiptRunOut, receiptSparse = db, out, true
	t.Cleanup(func() {
		receiptRunDB, receiptRunOut, receiptSparse, receiptFrom = defaultDBPath, "", false, 0
	})
	if err := runReceipt(testCommand(), []string{"run_sparse"}); err != nil {
		t.Fatalf("runReceipt(--sparse) error = %v", err)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	// The other tenant's paths are the readable part of their entries. A sparse receipt must not
	// carry them in any form.
	for _, p := range neighbors {
		if bytes.Contains(body, []byte(p)) {
			t.Errorf("the sparse receipt leaks a neighbor's entry: %s", p)
		}
	}
	if !bytes.Contains(body, []byte("loomseal-merkle-v1")) {
		t.Error("the sparse receipt is not on the tree profile")
	}
	if !bytes.Contains(body, []byte("run_sparse")) {
		t.Error("the sparse receipt does not name its own run")
	}
}
