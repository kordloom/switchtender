package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// apprReasonText is the approver reason the redaction tests decide with. It is the kind of text a
// redaction for personal data exists to remove.
const apprReasonText = "called the requester at home on 555 0100 before approving"

// apprFlakyRedactions is a decision store whose next Redact fails, as the update does when the
// database drops the connection or the process dies between the chain entry and the write.
type apprFlakyRedactions struct {
	decision.Store
	// mu guards failNext.
	mu sync.Mutex
	// failNext makes the next Redact fail without writing.
	failNext bool
}

// Redact fails once when armed and otherwise redacts.
func (f *apprFlakyRedactions) Redact(ctx context.Context, id string, red decision.Redaction) error {
	f.mu.Lock()
	fail := f.failNext
	f.failNext = false
	f.mu.Unlock()
	if fail {
		return errors.New("redact reason: connection reset by peer")
	}
	return f.Store.Redact(ctx, id, red)
}

// apprRedactionEntries returns the category of every redaction entry the chain holds for the
// decision with id, in chain order.
func apprRedactionEntries(t *testing.T, audits audit.Store, id string) []string {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []string
	for _, e := range chain {
		if e.Method != audit.MethodReason {
			continue
		}
		if entry, ok := decision.ParseReasonPath(e.Path); ok && entry.Redacted && entry.DecisionID == id {
			out = append(out, entry.Category)
		}
	}
	return out
}

// apprApprovedWithReason holds a run created under a recorded request, approves it with
// apprReasonText, waits for it to finish, and returns it with its decision record.
func apprApprovedWithReason(t *testing.T, d *Dispatcher, store run.Store,
	audits audit.Store) (*run.Run, *decision.Record) {
	t.Helper()
	creation := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "requester",
		ActorType: "session", Method: "POST", Path: "/v1/runs"}
	if err := audits.Append(context.Background(), creation); err != nil {
		t.Fatalf("append the creation request: %v", err)
	}
	ctx := run.WithAuditReceipt(context.Background(), audit.Receipt(creation))
	held, err := d.Submit(ctx, "play.yml", "inv", run.WithRequireApproval(true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if _, err := d.DecideRun(ctx, held.ID, RunDecision{Approve: true, Reason: apprReasonText,
		By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
		t.Fatalf("DecideRun() error = %v", err)
	}
	waitTerminal(t, store, held.ID)
	apprWaitOutcome(t, audits, held.ID)
	records, err := d.Decisions(context.Background(), held.ID)
	if err != nil || len(records) != 1 || records[0].Reason == nil ||
		records[0].Reason.Text != apprReasonText {
		t.Fatalf("Decisions() = (%+v, %v), want one approval carrying the reason", records, err)
	}
	return held, records[0]
}

// TestReasonRedactionThatFailsLeavesTheChainAndTheRecordAgreeing redacts an approver's reason for
// personal data, an erasure request, when the update that removes the text fails after the
// redaction's chain entry was appended. RedactReason appends the entry first and writes the store
// second with nothing tying the two together, so the chain now says the text was removed while the
// store still holds it, and every receipt drawn afterward discloses the reason the chain says is
// gone. The admin sees an error and retries, and the retry appends a second redaction entry for
// the one reason, because nothing reads the first.
func TestReasonRedactionThatFailsLeavesTheChainAndTheRecordAgreeing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	records := &apprFlakyRedactions{Store: decision.NewMemStore()}
	d := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithAudits(audits),
		WithDecisions(records))
	defer d.Close()
	held, rec := apprApprovedWithReason(t, d, store, audits)
	privacy := outcome.Decider{Name: "privacy-officer", Type: "session"}

	records.mu.Lock()
	records.failNext = true
	records.mu.Unlock()
	if _, err := d.RedactReason(ctx, held.ID, rec.ID, decision.CategoryPersonalData,
		privacy); err == nil {
		t.Fatal("RedactReason() succeeded through a failed update, want the failure reported")
	}
	after, err := records.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	entries := apprRedactionEntries(t, audits, rec.ID)
	if len(entries) > 0 && after.Reason.Text != "" {
		id, ierr := audit.LoadIdentity(t.TempDir())
		if ierr != nil {
			t.Fatalf("LoadIdentity() error = %v", ierr)
		}
		res, berr := receipt.Build(ctx, store, audits, id, "test", held.ID,
			receipt.Options{Decisions: records})
		if berr != nil {
			t.Fatalf("receipt.Build() error = %v", berr)
		}
		t.Errorf("after a failed redaction the chain records the reason as removed (%v) while the "+
			"store still holds it, and a receipt drawn now discloses it: %v", entries,
			strings.Contains(string(res.Signed), apprReasonText))
	}

	if _, err := d.RedactReason(ctx, held.ID, rec.ID, decision.CategoryPersonalData,
		privacy); err != nil {
		t.Fatalf("retried RedactReason() error = %v", err)
	}
	want := []string{decision.CategoryPersonalData}
	if diff := cmp.Diff(want, apprRedactionEntries(t, audits, rec.ID)); diff != "" {
		t.Errorf("redaction entries for one reason redacted once (-want +got):\n%s", diff)
	}
}

// TestReasonRedactionsRacingLeaveOneRedactionOnTheChain sends two redactions of one reason from two
// replicas, one for personal data and one as a secret. Both read the record as not yet redacted and
// append their chain entries; the store's conditional update lets exactly one remove the text, and
// the other is answered that the reason was already redacted. The chain keeps both entries, so it
// records two removals under two different causes for one reason, while the record names one. The
// PostgreSQL store's comment promises two redactions racing cannot both succeed. Either redaction
// may be the one that takes effect, whichever claims the record first; the chain has to hold that
// one and no other.
func TestReasonRedactionsRacingLeaveOneRedactionOnTheChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	records := decision.NewMemStore()
	gated := apprNewGatedAudits(audits, "/reason_redacted/"+decision.CategorySecret)
	replicaA := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithAudits(gated),
		WithDecisions(records), WithOwner("replica-a"))
	defer replicaA.Close()
	replicaB := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithAudits(audits),
		WithDecisions(records), WithOwner("replica-b"))
	defer replicaB.Close()
	held, rec := apprApprovedWithReason(t, replicaB, store, audits)

	slow := make(chan error, 1)
	go func() {
		_, err := replicaA.RedactReason(ctx, held.ID, rec.ID, decision.CategorySecret,
			outcome.Decider{Name: "security-admin", Type: "session"})
		slow <- err
	}()
	apprWait(t, gated.reached, "the first redaction to pass its checks")
	_, errB := replicaB.RedactReason(ctx, held.ID, rec.ID, decision.CategoryPersonalData,
		outcome.Decider{Name: "privacy-officer", Type: "session"})
	close(gated.release)
	errA := <-slow
	refused := errA
	if errA == nil {
		refused = errB
	}
	if (errA == nil) == (errB == nil) || !errors.Is(refused, decision.ErrRedacted) {
		t.Fatalf("the racing redactions returned %v and %v, want one to take effect and the "+
			"other refused with ErrRedacted", errA, errB)
	}
	after, err := records.Get(ctx, rec.ID)
	if err != nil || !after.HasReason() || after.Reason.Redacted == nil {
		t.Fatalf("Get() = (%+v, %v), want the reason redacted", after, err)
	}
	want := []string{after.Reason.Redacted.Category}
	if diff := cmp.Diff(want, apprRedactionEntries(t, audits, rec.ID)); diff != "" {
		t.Errorf("redaction entries for a reason the store redacted once (-want +got):\n%s", diff)
	}
}
