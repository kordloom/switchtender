package dispatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// schClock is a settable clock handed to the dispatcher with WithClock, so a test can let hours
// pass inside a run without waiting for them.
type schClock struct {
	// mu guards at.
	mu sync.Mutex
	// at is the time the clock reads.
	at time.Time
}

// now reads the clock.
func (c *schClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// advance moves the clock forward by d.
func (c *schClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// schServedHosts lists the hosts a run found in its cache directory in the layout ansible-core
// 2.18 and earlier read, one file named after each host.
func schServedHosts(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), "s1_") {
			out = append(out, f.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// schFactInventory returns an inventory store holding inv_1, whose one host is web01.
func schFactInventory(t *testing.T) inventory.Store {
	t.Helper()
	inventories := inventory.NewMemStore()
	if err := inventories.Save(context.Background(), &inventory.Inventory{ID: "inv_1",
		Name: "fleet", Content: "[web]\nweb01\n", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return inventories
}

// TestFactCacheServesFactsOlderThanTheTimeoutAfterALongRun runs a play that gathers web01's facts
// as its first task and then runs for five more hours, and launches the next run of the same
// template half an hour after the first one ends, with a one hour fact cache timeout.
//
// The Ansible guide promises that fact_cache_timeout is how many seconds cached facts stay fresh
// enough to serve, and that a host whose facts are older is left out of the cache so the play
// gathers it again. The facts were gathered five and a half hours before the second run. They are
// served anyway, because the dispatcher stamps every collected document with the time the run
// ended rather than the time Ansible wrote it, so the timeout is measured from the end of the run
// that gathered the facts and a long run hands the next one facts as old as itself.
func TestFactCacheServesFactsOlderThanTheTimeoutAfterALongRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	facts := factcache.NewMemStore()
	clock := &schClock{at: time.Now()}
	var mu sync.Mutex
	gather := true
	var served []string
	runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		hosts, err := schServedHosts(spec.FactCacheDir)
		if err != nil {
			return roundhouse.Result{ExitCode: 1}, err
		}
		served = hosts
		if !gather || spec.FactCacheDir == "" {
			return roundhouse.Result{}, nil
		}
		// gather_facts runs first and the jsonfile plugin writes web01's file at once.
		if err := os.WriteFile(filepath.Join(spec.FactCacheDir, "web01"),
			[]byte(`{"ansible_distribution":"Debian","ansible_default_ipv4":"10.0.0.5"}`),
			0o600); err != nil {
			return roundhouse.Result{ExitCode: 1}, err
		}
		// The rest of the play takes five hours.
		clock.advance(5 * time.Hour)
		return roundhouse.Result{}, nil
	})
	d := New(store, runner, nil, WithNoJanitor(), WithInventories(schFactInventory(t)),
		WithFactCache(facts), WithClock(clock.now))
	defer d.Close()
	launch := func() *run.Run {
		t.Helper()
		r, err := d.Submit(ctx, "patch.yml", "", run.WithInventory("inv_1"),
			run.WithFactCache(true, 3600))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		return waitTerminal(t, store, r.ID)
	}

	if first := launch(); first.Status != run.StatusSucceeded {
		t.Fatalf("the gathering run = %s: %s", first.Status, first.Error)
	}
	stored, err := facts.Facts(ctx, "inv_1", "web01")
	if err != nil {
		t.Fatalf("the gathering run kept no facts for web01: %v", err)
	}
	clock.advance(30 * time.Minute)
	mu.Lock()
	gather = false
	mu.Unlock()
	launch()

	mu.Lock()
	defer mu.Unlock()
	if diff := cmp.Diff([]string(nil), served, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a run with a one hour timeout was served facts gathered five and a half hours "+
			"earlier, stamped %s, which is when the gathering run ended (-want +got):\n%s",
			stored.ModifiedAt.Format(time.RFC3339), diff)
	}
}

// TestFactCacheOlderGatherFinishingLaterOverwritesNewerFacts runs two launches of a fact caching
// template against one inventory at once: the first gathers web01 and then keeps running, the
// second gathers web01 afterward and finishes first.
//
// The guide says the next launch is served the facts each launch gathers, and that a cached fact
// stands for the host as it was when the facts were cached. Every save replaces the stored document
// whatever its age, so the first launch, finishing last, writes the older gather over the newer one
// and stamps it as the newest. Every later run is served the host as it was before the second
// launch looked at it, with nothing on the record to say the newer facts existed.
func TestFactCacheOlderGatherFinishingLaterOverwritesNewerFacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	facts := factcache.NewMemStore()
	slowGathered := make(chan struct{})
	releaseSlow := make(chan struct{})
	runner := roundhouse.RunnerFunc(func(ctx context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		if spec.FactCacheDir == "" {
			return roundhouse.Result{ExitCode: 1}, nil
		}
		path := filepath.Join(spec.FactCacheDir, "web01")
		switch spec.Playbook {
		case "slow.yml":
			if err := os.WriteFile(path, []byte(`{"kernel":"6.1.0-old"}`), 0o600); err != nil {
				return roundhouse.Result{ExitCode: 1}, err
			}
			close(slowGathered)
			select {
			case <-releaseSlow:
			case <-ctx.Done():
				return roundhouse.Result{ExitCode: 1}, ctx.Err()
			}
		case "quick.yml":
			if err := os.WriteFile(path, []byte(`{"kernel":"6.8.0-new"}`), 0o600); err != nil {
				return roundhouse.Result{ExitCode: 1}, err
			}
		}
		return roundhouse.Result{}, nil
	})
	d := New(store, runner, nil, WithNoJanitor(), WithInventories(schFactInventory(t)),
		WithFactCache(facts), WithWorkers(4))
	defer d.Close()
	submit := func(playbook string) *run.Run {
		t.Helper()
		r, err := d.Submit(ctx, playbook, "", run.WithInventory("inv_1"),
			run.WithFactCache(true, 0))
		if err != nil {
			t.Fatalf("Submit(%s) error = %v", playbook, err)
		}
		return r
	}

	slow := submit("slow.yml")
	select {
	case <-slowGathered:
	case <-time.After(waitBudget):
		t.Fatal("the slow run never gathered")
	}
	quick := waitTerminal(t, store, submit("quick.yml").ID)
	if quick.Status != run.StatusSucceeded {
		t.Fatalf("the quick run = %s: %s", quick.Status, quick.Error)
	}
	newest, err := facts.Facts(ctx, "inv_1", "web01")
	if err != nil {
		t.Fatalf("the quick run kept no facts: %v", err)
	}
	close(releaseSlow)
	if done := waitTerminal(t, store, slow.ID); done.Status != run.StatusSucceeded {
		t.Fatalf("the slow run = %s: %s", done.Status, done.Error)
	}

	got, err := facts.Facts(ctx, "inv_1", "web01")
	if err != nil {
		t.Fatalf("Facts() error = %v", err)
	}
	if diff := cmp.Diff(string(newest.Facts), string(got.Facts)); diff != "" {
		t.Errorf("web01's cached facts after both runs are the older gather, kept from run %s "+
			"over run %s's newer one (-want +got):\n%s", got.RunID, quick.ID, diff)
	}
}
