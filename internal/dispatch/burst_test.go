package dispatch

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDispatcherBurstAcrossDefaultWorkers runs the dispatcher in its production shape, the default
// worker pool and the stale-lease janitor both on, under a burst of concurrent submissions. Every other
// dispatcher test pins the pool to a single worker, so the semaphore accounting, the claim loop handing
// work to several executors at once, and the janitor ticking against rows workers are actively claiming
// have never been exercised together. A bug in any of them shows up as a dropped run, a run executed
// more than once, or a data race, so this asserts each run runs exactly once, all reach success, and it
// is meant to be run under -race.
func TestDispatcherBurstAcrossDefaultWorkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	execByPlaybook := make(map[string]int)
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			mu.Lock()
			execByPlaybook[spec.Playbook]++
			mu.Unlock()
			_, _ = io.WriteString(out, "PLAY RECAP *****\nweb01 : ok=1 changed=0\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})

	store := run.NewMemStore()
	// No options: the default four-worker pool and the janitor, which is the shape a real install runs.
	d := New(store, runner, nil)
	defer d.Close()

	const runs = 60
	ids := make([]string, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A distinct playbook per run keeps identical-spec dedup from collapsing the burst, so the
			// pool really does carry sixty separate runs.
			created, err := d.Submit(ctx, fmt.Sprintf("site-%02d.yml", i), "inv", run.WithActor("alice"))
			if err != nil {
				t.Errorf("Submit(%d) error = %v", i, err)
				return
			}
			ids[i] = created.ID
		}(i)
	}
	wg.Wait()

	for i, id := range ids {
		if id == "" {
			t.Fatalf("run %d was never submitted", i)
		}
		if got := waitTerminal(t, store, id); got.Status != run.StatusSucceeded {
			t.Errorf("run %s ended %s, want succeeded", id, got.Status)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(execByPlaybook) != runs {
		t.Errorf("the pool executed %d distinct runs, want %d (a missing one was dropped)",
			len(execByPlaybook), runs)
	}
	for pb, n := range execByPlaybook {
		if n != 1 {
			t.Errorf("%s executed %d times, want exactly once; more than once means the pool or the "+
				"janitor handed the same run to two workers", pb, n)
		}
	}
}
