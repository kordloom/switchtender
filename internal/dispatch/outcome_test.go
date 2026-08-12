package dispatch

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestFinalizeCommitsRunOutcomeToChain proves a finished run commits its outcome to the tamper-evident
// chain, and that the committed digest verifies against the run's actual outcome. The verification is
// the point: it is what makes the entry a commitment to what happened rather than an assertion a
// reader has to trust. Without it, the chain records only what was asked of the run.
func TestFinalizeCommitsRunOutcomeToChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, _ = io.WriteString(out, "PLAY RECAP *****\nweb01 : ok=1 changed=0\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	d := New(store, runner, nil, WithAudits(audits))
	defer d.Close()

	created, err := d.Submit(ctx, "site.yml", "inv", run.WithActor("alice"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := waitTerminal(t, store, created.ID)

	outcomeEntry := waitOutcomeEntry(t, audits, created.ID)
	if want := "/runs/" + created.ID + "/outcome/succeeded"; outcomeEntry.Path != want {
		t.Errorf("outcome path = %q, want %q", outcomeEntry.Path, want)
	}
	if outcomeEntry.ActorType != "system" {
		t.Errorf("outcome actor type = %q, want system", outcomeEntry.ActorType)
	}
	if outcomeEntry.OnBehalfOf != "alice" {
		t.Errorf("outcome on_behalf_of = %q, want alice", outcomeEntry.OnBehalfOf)
	}
	if outcomeEntry.ContentDigest == "" {
		t.Fatal("outcome entry carries no content digest")
	}

	// The committed digest must verify against the run's real outcome, reconstructed from the store.
	body, err := outcome.Body(ctx, store, got)
	if err != nil {
		t.Fatalf("outcomeBody() error = %v", err)
	}
	if !audit.VerifyContentDigest(outcomeEntry.ContentDigest, outcomeEntry.Nonce, body) {
		t.Error("committed outcome digest does not verify against the run's actual outcome")
	}
	// A tampered body must not verify, or the commitment proves nothing.
	if audit.VerifyContentDigest(outcomeEntry.ContentDigest, outcomeEntry.Nonce, append(body, '!')) {
		t.Error("a tampered outcome body verified against the committed digest")
	}
}

// TestNoAuditsNoOutcomeEntry checks the commitment is opt-in: a dispatcher with no audit chain runs
// exactly as before and writes nothing.
func TestNoAuditsNoOutcomeEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	store := run.NewMemStore()
	d := New(store, runner, nil) // no WithAudits
	defer d.Close()

	created, err := d.Submit(ctx, "site.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	// Reaching a terminal state without a nil-audits panic is the assertion.
	waitTerminal(t, store, created.ID)
}

// waitOutcomeEntry polls the chain for the run's outcome entry and returns it, failing at the
// deadline if it never arrives.
//
// The entry is committed after the terminal status is saved, deliberately: the append is not
// fail-closed, because the run has already happened and a chain that cannot record it is logged
// rather than pretended away. So a run reading as terminal does not yet mean its outcome has landed,
// and reading the chain at that instant made this test report a failure about correct behavior under
// load. Polling asserts the property the product actually promises, that a finished run commits its
// outcome, rather than the instant the test happened to look.
func waitOutcomeEntry(t *testing.T, audits audit.Store, runID string) *audit.Entry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		chain, err := audits.Chain(context.Background())
		if err != nil {
			t.Fatalf("Chain() error = %v", err)
		}
		for _, e := range chain {
			if e.Method == audit.MethodRun && strings.Contains(e.Path, runID) {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no outcome entry was committed for the finished run")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
