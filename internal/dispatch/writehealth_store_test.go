package dispatch_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// errStoreFull stands in for a store that answers reads and refuses writes, a full disk.
var errStoreFull = errors.New("database or disk is full (13)")

// refusingStore refuses terminal writes while refuse is set and counts the heartbeats it is sent.
type refusingStore struct {
	run.Store
	// mu guards the fields below.
	mu sync.Mutex
	// refuse makes FinalizeRunning and Save fail.
	refuse bool
	// beats counts heartbeats per run id.
	beats map[string]int
}

// setRefuse turns refusal on or off.
func (s *refusingStore) setRefuse(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse = on
}

// refusing reports whether writes are refused.
func (s *refusingStore) refusing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refuse
}

// FinalizeRunning refuses while refusal is on.
func (s *refusingStore) FinalizeRunning(ctx context.Context, id string, fin run.Finalization) (bool, error) {
	if s.refusing() {
		return false, errStoreFull
	}
	return s.Store.FinalizeRunning(ctx, id, fin)
}

// Save refuses while refusal is on.
func (s *refusingStore) Save(ctx context.Context, r *run.Run) error {
	if s.refusing() {
		return errStoreFull
	}
	return s.Store.Save(ctx, r)
}

// Heartbeat counts the beat and passes it on.
func (s *refusingStore) Heartbeat(ctx context.Context, id, owner string) error {
	s.mu.Lock()
	if s.beats == nil {
		s.beats = map[string]int{}
	}
	s.beats[id]++
	s.mu.Unlock()
	return s.Store.Heartbeat(ctx, id, owner)
}

// resetBeats forgets the heartbeats counted so far.
func (s *refusingStore) resetBeats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats = nil
}

// beatsFor returns how many heartbeats id was sent.
func (s *refusingStore) beatsFor(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beats[id]
}

// TestARefusedEndLandsWhenTheStoreAnswers pins the run whose terminal write the store refuses, on
// every store: it reads as running while the store refuses, readiness reports the end owed, and once
// the store takes writes the end that lands is the run's true one, with a warning saying it was
// recorded late and why. The run's lease is renewed while the end is owed, so another replica's
// janitor does not mark interrupted a run whose end this process holds. Before, the end was dropped:
// the run stayed running until a janitor could write, and then ended interrupted, its exit code lost.
func TestARefusedEndLandsWhenTheStoreAnswers(t *testing.T) {
	t.Parallel()
	for _, backend := range leaseBackends() {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := &refusingStore{Store: backend.Open(t)}
			// The tool's own work finishes, then the disk fills before the end is recorded.
			runner := roundhouse.RunnerFunc(
				func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
					_, _ = io.WriteString(out, "PLAY RECAP *****\nweb01 : ok=1 changed=1\n")
					store.setRefuse(true)
					return roundhouse.Result{ExitCode: 0}, nil
				})
			d := dispatch.New(store, runner, nil, dispatch.WithNoJanitor())
			defer d.Close()
			created, err := d.Submit(ctx, "site.yml", "hosts.ini")
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for d.Health().OwedEnds == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			health := d.Health()
			if !health.Degraded || health.OwedEnds != 1 {
				t.Fatalf("Health() = %+v, want degraded with one end owed", health)
			}
			if got, err := store.Get(ctx, created.ID); err != nil || got.Status != run.StatusRunning {
				t.Fatalf("stored run = %v, %v, want running while the store refuses", got, err)
			}

			// Only the beats sent for the owed end count, not the executor's while the run went.
			store.resetBeats()
			store.setRefuse(false)
			d.LandOwedEnds()
			got, err := store.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != run.StatusSucceeded || got.ExitCode == nil || *got.ExitCode != 0 {
				t.Errorf("stored run = %s exit %v, want succeeded exit 0", got.Status, got.ExitCode)
			}
			if !strings.Contains(got.Warning, "recorded late, after the store refused the write: "+
				errStoreFull.Error()) {
				t.Errorf("warning = %q, want it to say the end was recorded late and why", got.Warning)
			}
			if store.beatsFor(created.ID) == 0 {
				t.Error("the owed run's lease was never renewed")
			}
			if n := d.Health().OwedEnds; n != 0 {
				t.Errorf("OwedEnds = %d after the end landed, want 0", n)
			}
		})
	}
}

// TestAFencedEndIsNeverOwed pins that only a store's refusal is held for retry. A run another worker
// holds is not this worker's to finalize, and holding that end would overwrite the other worker's
// run once the store answered.
func TestAFencedEndIsNeverOwed(t *testing.T) {
	t.Parallel()
	for _, backend := range leaseBackends() {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := backend.Open(t)
			idle := roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{}, nil
				})
			d := dispatch.New(store, idle, nil, dispatch.WithNoJanitor())
			defer d.Close()
			// Stored running under another worker, so this dispatcher's claim loop never takes it.
			claimed := &run.Run{ID: "run_other", Playbook: "site.yml", Status: run.StatusRunning,
				ClaimedBy: "worker-b", CreatedAt: time.Now()}
			if err := store.Save(ctx, claimed); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			mine := claimed.Clone()
			mine.ClaimedBy = "worker-a"
			d.FinalizeForTest(mine, run.StatusSucceeded)
			if n := d.Health().OwedEnds; n != 0 {
				t.Errorf("OwedEnds = %d for a run another worker holds, want 0", n)
			}
			got, err := store.Get(ctx, claimed.ID)
			if err != nil || got.Status != run.StatusRunning || got.ClaimedBy != "worker-b" {
				t.Errorf("stored run = %v, %v, want running under worker-b", got, err)
			}
		})
	}
}
