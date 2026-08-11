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
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
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
		Actor: "alice", AuditReceipt: audit.Receipt(creation), CreatedAt: at,
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	if err := store.Runs().AppendLog(ctx, "run_demo", []byte("PLAY RECAP\nweb01 : ok=5 changed=1\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := store.Runs().SaveHostSummary(ctx, "run_demo",
		[]run.HostSummary{{Host: "web01", OK: 5, Changed: 1, Worst: "changed"}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
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
	t.Logf("switchtender verify run.receipt:\n%s", buf.String())

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
