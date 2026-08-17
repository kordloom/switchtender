package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestAWorkerCommitsTheOutcomesItExecutes covers the evidence hole that opens the moment a deployment
// scales past one process, which is the topology the product tells customers to scale into.
//
// A worker sharing the database wires credentials, projects, inventories, sources, and policies, and it
// did not wire the audit store. The dispatcher's finalize commits a run's outcome to the tamper-evident
// chain only when it has one, so every run a worker executed finished with no outcome entry: not
// receiptable, absent from its own dossier, and invisible to the offline verification the whole product
// rests on. Measured on a live four-process deployment, 172 of 200 runs had no evidence at all, and the
// runs that did were only the ones the control node happened to claim. Nothing failed, which is what
// made it dangerous: the deployment looked healthy and the evidence was simply missing.
func TestAWorkerCommitsTheOutcomesItExecutes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "switchtender.db")

	// The request that created the run is recorded on the chain first, exactly as the API's audit gate
	// records it, and the run carries that receipt. A run submitted straight through a dispatcher has
	// no such entry, and a receipt cannot place its start on the chain without one.
	seed, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open store for the creation entry: %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := seed.Audits().Append(ctx, creation); err != nil {
		t.Fatalf("append creation entry: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	createdReceipt := fmt.Sprintf("%d:%s", creation.Seq, creation.Hash)

	// The worker's own store and options, built exactly as the command builds them.
	prev := workerDB
	workerDB = dbPath
	t.Cleanup(func() { workerDB = prev })
	store, opts, closeStore, err := workerStore(zap.NewNop())
	if err != nil {
		t.Fatalf("workerStore: %v", err)
	}
	t.Cleanup(closeStore)

	d := dispatch.New(store, workerTestRunner(), zap.NewNop(), opts...)
	t.Cleanup(d.Close)

	// A run the worker claims and executes, with the creation receipt a submitted run carries.
	created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("echo hi"),
		run.WithActor("casey"), run.WithActorType("session"),
		run.WithAuditReceiptOf(createdReceipt))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var finished *run.Run
	for time.Now().Before(deadline) {
		got, err := store.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status.Terminal() {
			finished = got
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if finished == nil {
		t.Fatal("the run never reached a terminal state")
	}

	// The evidence: the chain has to hold this run's outcome, and a receipt has to build from it.
	db, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	chain, err := db.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	var outcomes int
	for _, e := range chain {
		if strings.Contains(e.Path, created.ID) && strings.Contains(e.Path, "/outcome/") {
			outcomes++
		}
	}
	if outcomes != 1 {
		t.Fatalf("the chain holds %d outcome entries for a run a worker executed, want exactly 1: "+
			"without one the run cannot be receipted and its evidence does not exist", outcomes)
	}

	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if _, err := receipt.Build(ctx, db.Runs(), db.Audits(), id, "test", created.ID,
		receipt.Options{}); err != nil {
		t.Fatalf("a run a worker executed cannot be receipted: %v", err)
	}
}

// workerTestRunner executes nothing and reports success, so the test measures what the dispatcher
// records rather than what a shell does.
func workerTestRunner() roundhouse.Runner {
	return roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, _ = io.WriteString(out, "ok\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})
}
