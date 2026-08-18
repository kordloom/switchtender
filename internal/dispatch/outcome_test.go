package dispatch

import (
	"context"
	"io"
	"strings"
	"sync"
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

// TestClockSeedsRunAndOutcomeInThePast proves WithClock parks a run's whole history in the past: the
// created, started, and ended times on its record, and the time the audit entry claims for its
// outcome, all read the injected clock rather than the wall clock. This is what lets the demo seed a
// run as of hours ago with its record, its chain entry, and the receipt built from that entry all
// agreeing on when it ran. A production dispatcher passes no clock and keeps time.Now.
func TestClockSeedsRunAndOutcomeInThePast(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, _ = io.WriteString(out, "PLAY RECAP *****\nweb01 : ok=1 changed=0\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})
	store := run.NewMemStore()
	audits := audit.NewMemStore()

	// A clock parked eight hours back that advances a millisecond a call, so every stamp lands in the
	// past and strictly after the one before it, the way a real clock would over the life of one run.
	base := time.Now().Add(-8 * time.Hour)
	var mu sync.Mutex
	var seq int64
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return base.Add(time.Duration(seq) * time.Millisecond)
	}
	d := New(store, runner, nil, WithAudits(audits), WithClock(clock))
	defer d.Close()

	created, err := d.Submit(ctx, "site.yml", "inv", run.WithActor("alice"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := waitTerminal(t, store, created.ID)

	// Every record stamp lands well in the injected past, not at the real wall clock.
	recent := time.Now().Add(-7 * time.Hour)
	if !got.CreatedAt.Before(recent) {
		t.Errorf("created_at = %s, want before %s (the injected past)", got.CreatedAt, recent)
	}
	if got.StartedAt == nil || got.EndedAt == nil {
		t.Fatalf("run has no start or end: started=%v ended=%v", got.StartedAt, got.EndedAt)
	}
	if !got.StartedAt.Before(recent) || !got.EndedAt.Before(recent) {
		t.Errorf("started=%s ended=%s, want both before %s", got.StartedAt, got.EndedAt, recent)
	}
	// The lifecycle stays ordered within the injected clock: created, then started, then ended.
	if got.StartedAt.Before(got.CreatedAt) || got.EndedAt.Before(*got.StartedAt) {
		t.Errorf("lifecycle out of order: created=%s started=%s ended=%s",
			got.CreatedAt, got.StartedAt, got.EndedAt)
	}

	// The outcome entry is stamped from the same clock, so the chain and the receipt built from it
	// reconcile with the record instead of claiming the run finished just now.
	outcomeEntry := waitOutcomeEntry(t, audits, created.ID)
	if !outcomeEntry.At.Before(recent) {
		t.Errorf("outcome entry at = %s, want in the injected past before %s", outcomeEntry.At, recent)
	}
	if outcomeEntry.At.Before(*got.EndedAt) {
		t.Errorf("outcome at %s precedes the run's end %s", outcomeEntry.At, got.EndedAt)
	}
	if outcomeEntry.At.Sub(*got.EndedAt) > time.Minute {
		t.Errorf("outcome at %s is more than a minute past end %s; not reconciled",
			outcomeEntry.At, got.EndedAt)
	}
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
