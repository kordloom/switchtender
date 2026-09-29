package schedule

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// receiptSubmitter records the audit receipt carried on the submit context, which is what the
// dispatcher stamps onto the run it creates.
type receiptSubmitter struct {
	// receipt is the audit receipt seen on the last submit.
	receipt string
}

// Submit records the context's audit receipt and returns a stub run.
func (f *receiptSubmitter) Submit(ctx context.Context, _, _ string, _ ...run.SubmitOption) (*run.Run, error) {
	f.receipt = run.AuditReceiptFrom(ctx)
	return &run.Run{ID: "run_sched"}, nil
}

// SubmitSplit records the context's audit receipt and returns a stub run.
func (f *receiptSubmitter) SubmitSplit(ctx context.Context, _, _ string, _ int, _ ...run.SubmitOption) (*run.Run, error) {
	f.receipt = run.AuditReceiptFrom(ctx)
	return &run.Run{ID: "run_sched"}, nil
}

// SubmitPipeline records the context's audit receipt and returns a stub run.
func (f *receiptSubmitter) SubmitPipeline(ctx context.Context, _, _ string, _ []run.PipelineStep, _ ...run.SubmitOption) (*run.Run, error) {
	f.receipt = run.AuditReceiptFrom(ctx)
	return &run.Run{ID: "run_sched"}, nil
}

// failingAudits refuses every append, standing in for an unhealthy chain.
type failingAudits struct {
	audit.Store
}

// Append refuses, so the fire must be refused too.
func (failingAudits) Append(context.Context, *audit.Entry) error {
	return errors.New("chain unavailable")
}

// TestScheduledFireIsOnTheChain proves a scheduler fire is recorded as a chain entry before the run
// exists, and the run's submit context carries that entry's receipt, so a scheduled run has the
// same creation evidence as one a person requested.
func TestScheduledFireIsOnTheChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	sub := &receiptSubmitter{}
	// The loop is never started; fire is exercised directly, so there is nothing to Close.
	s := NewScheduler(NewMemStore(), sub, zap.NewNop(), WithAudits(audits))

	sc := &Schedule{ID: "sch_1", Name: "nightly", Playbook: "site.yml", Inventory: "prod"}
	if _, err := s.fire(ctx, sc); err != nil {
		t.Fatalf("fire() error = %v", err)
	}
	if sub.receipt == "" {
		t.Fatal("the submit context carried no audit receipt, so the run cannot be receipted")
	}
	entries, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("chain holds %d entries, want the one fire entry", len(entries))
	}
	e := entries[0]
	if e.Method != audit.MethodSchedule || e.Path != "/schedules/sch_1/fired" {
		t.Errorf("entry = %s %s, want SCHEDULE /schedules/sch_1/fired", e.Method, e.Path)
	}
	if e.Actor != "system:scheduler" || e.ActorType != "system" {
		t.Errorf("entry actor = %s (%s), want system:scheduler (system)", e.Actor, e.ActorType)
	}
	if e.ContentDigest == "" {
		t.Error("the fire entry commits no content digest, so what fired is not committed")
	}
	if want := audit.Receipt(e); sub.receipt != want {
		t.Errorf("submit receipt = %q, want %q", sub.receipt, want)
	}
}

// TestScheduledFireFailsClosed proves a fire that cannot be recorded is skipped rather than
// performed silently, the same rule the API gate applies to every mutation.
func TestScheduledFireFailsClosed(t *testing.T) {
	t.Parallel()
	sub := &receiptSubmitter{}
	// The loop is never started; fire is exercised directly, so there is nothing to Close.
	s := NewScheduler(NewMemStore(), sub, zap.NewNop(), WithAudits(failingAudits{}))

	sc := &Schedule{ID: "sch_2", Name: "nightly", Playbook: "site.yml"}
	if _, err := s.fire(context.Background(), sc); err == nil {
		t.Fatal("fire() succeeded with a chain that refuses appends")
	}
	if sub.receipt != "" {
		t.Error("the submitter was called for a fire that could not be recorded")
	}
}
