package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// audDroppedOutcome is an audit store that refuses the first outcome entry offered to it, the way a
// database failover, a lock timeout, or a connection reset refuses one append, and accepts
// everything after. It counts how many outcome entries it refused.
type audDroppedOutcome struct {
	audit.Store
	// mu guards refused.
	mu sync.Mutex
	// refused counts the outcome entries refused so far.
	refused int
}

// Append refuses the first RUN outcome entry and passes every other append through.
func (s *audDroppedOutcome) Append(ctx context.Context, e *audit.Entry) error {
	if e.Method == audit.MethodRun && strings.Contains(e.Path, "/outcome/") {
		s.mu.Lock()
		first := s.refused == 0
		if first {
			s.refused++
		}
		s.mu.Unlock()
		if first {
			return errors.New("append audit entry: read tcp: connection reset by peer")
		}
	}
	return s.Store.Append(ctx, e)
}

// refusals returns how many outcome entries the store has refused.
func (s *audDroppedOutcome) refusals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// audHasOutcome reports whether the chain holds an outcome entry for run id.
func audHasOutcome(t *testing.T, audits audit.Store, id string) bool {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
			return true
		}
	}
	return false
}

// TestChainEventuallyHoldsTheOutcomeOfEveryFinishedRun finishes a run whose outcome entry the chain
// refuses once, then starts the process that comes after it, a restart or a surviving replica with
// its janitor running, and waits two and a half janitor periods for the outcome to reach the chain.
//
// The reliability page promises that every process that finishes a run commits that run's outcome,
// including the runs nobody is left to finish, because a change that executed is exactly the
// incident somebody asks about afterward. commitOutcome writes the terminal record first and the
// chain entry second, logs a refused append, and drops it; the janitor commits outcomes only for
// runs it settled itself. So one refused append, or a process killed between the two writes, leaves
// a run the database says succeeded with no outcome anywhere in the chain, forever. The chain still
// verifies, because an entry that was never appended leaves no gap, and the run's receipt verifies
// too while saying nothing about what the run did.
func TestChainEventuallyHoldsTheOutcomeOfEveryFinishedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := &audDroppedOutcome{Store: audit.NewMemStore()}

	first := New(runs, okRunner(), nil, WithAudits(audits), WithNoJanitor(),
		WithOwner("replica-a"))
	const id = "run_outcome_dropped"
	if err := runs.Save(ctx, &run.Run{
		ID: id, Playbook: "site.yml", Inventory: "prod", Status: run.StatusPending,
		CreatedAt: time.Now(), Actor: "operator",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if done := waitTerminal(t, runs, id); done.Status != run.StatusSucceeded {
		t.Fatalf("the run ended %s, want succeeded", done.Status)
	}
	first.Close()
	if got := audits.refusals(); got != 1 {
		t.Fatalf("the store refused %d outcome entries, want exactly the 1 this test drops", got)
	}

	// The process that comes next, with its janitor sweeping on start and every period after.
	next := New(runs, okRunner(), nil, WithAudits(audits), WithOwner("replica-b"))
	defer next.Close()
	deadline := time.Now().Add(janitorInterval*2 + janitorInterval/2)
	for time.Now().Before(deadline) {
		if audHasOutcome(t, audits, id) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("run %s is %s in the store and the chain holds no outcome for it %s after a "+
		"janitor started: a single refused append lost the record of what the run did",
		id, run.StatusSucceeded, janitorInterval*2+janitorInterval/2)
}
