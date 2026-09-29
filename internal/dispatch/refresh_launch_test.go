package dispatch

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// countingDumper records how many inventory dumps were asked for and is safe to read from a test
// while the refresh path runs.
type countingDumper struct {
	// mu guards sources.
	mu sync.Mutex
	// sources holds every source path handed to Dump.
	sources []string
}

// Run satisfies roundhouse.Runner; the refresh path never executes a playbook.
func (c *countingDumper) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 0}, nil
}

// Dump records the path and returns an empty but parseable inventory.
func (c *countingDumper) Dump(_ context.Context, source string, _ []string) ([]byte, error) {
	c.mu.Lock()
	c.sources = append(c.sources, source)
	c.mu.Unlock()
	return []byte(`{"_meta":{"hostvars":{}},"all":{"children":[]}}`), nil
}

// count returns how many dumps were requested.
func (c *countingDumper) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sources)
}

// TestSourceDueBoundaries pins the scheduled-sync clock. A source that is due too eagerly re-runs an
// inventory plugin against a cloud API on every tick, and one that is never due leaves the fleet
// working from a snapshot nobody notices is stale.
func TestSourceDueBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		// Name says which schedule is being read.
		Name string
		// Interval is the source's configured sync interval in seconds.
		Interval int
		// SyncedAt is when it last synced, or nil when it never has.
		SyncedAt *time.Time
		// WantDue is whether the scheduled sync should refresh it now.
		WantDue bool
	}{{ // Test 0: No interval means no scheduled sync, however long ago it synced.
		Name: "no interval", Interval: 0, SyncedAt: ptr(now.Add(-24 * time.Hour)), WantDue: false,
	}, { // Test 1: A negative interval is not a schedule either.
		Name: "a negative interval", Interval: -60, SyncedAt: nil, WantDue: false,
	}, { // Test 2: A scheduled source that has never synced is due at once.
		Name: "never synced", Interval: 300, SyncedAt: nil, WantDue: true,
	}, { // Test 3: Exactly at the interval is due.
		Name: "exactly at the interval", Interval: 300,
		SyncedAt: ptr(now.Add(-300 * time.Second)), WantDue: true,
	}, { // Test 4: One nanosecond short is not.
		Name: "one nanosecond short", Interval: 300,
		SyncedAt: ptr(now.Add(-300*time.Second + time.Nanosecond)), WantDue: false,
	}, { // Test 5: A source synced in the future, from a clock skew, is not due.
		Name: "synced in the future", Interval: 300, SyncedAt: ptr(now.Add(time.Hour)),
		WantDue: false,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			src := &invsource.Source{SyncIntervalSeconds: test.Interval, SyncedAt: test.SyncedAt}
			if got := sourceDue(src, now); got != test.WantDue {
				t.Errorf("sourceDue() = %v, want %v", got, test.WantDue)
			}
		})
	}
}

// TestLaunchStaleBoundaries pins the other clock, which decides whether a run refreshes its dynamic
// inventory before it starts. It reads a zero interval the opposite way from the scheduled sync: no
// interval means refresh on every launch, so a run always sees current hosts, rather than never.
func TestLaunchStaleBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		// Name says which schedule is being read.
		Name string
		// Interval is the source's configured sync interval in seconds.
		Interval int
		// SyncedAt is when it last synced, or nil when it never has.
		SyncedAt *time.Time
		// WantStale is whether a launch should refresh it first.
		WantStale bool
	}{{ // Test 0: No interval refreshes on every launch, unlike the scheduled sync.
		Name: "no interval", Interval: 0, SyncedAt: ptr(now), WantStale: true,
	}, { // Test 1: A source that has never synced is stale whatever its interval.
		Name: "never synced", Interval: 3600, SyncedAt: nil, WantStale: true,
	}, { // Test 2: Exactly at the interval is stale.
		Name: "exactly at the interval", Interval: 300,
		SyncedAt: ptr(now.Add(-300 * time.Second)), WantStale: true,
	}, { // Test 3: Just inside the interval is fresh, so a burst of launches does not re-dump.
		Name: "just inside the interval", Interval: 300,
		SyncedAt: ptr(now.Add(-299 * time.Second)), WantStale: false,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			src := &invsource.Source{SyncIntervalSeconds: test.Interval, SyncedAt: test.SyncedAt}
			if got := launchStale(src, now); got != test.WantStale {
				t.Errorf("launchStale() = %v, want %v", got, test.WantStale)
			}
		})
	}
}

// launchSetup builds a dispatcher wired for inventory-source refreshes, with one stored inventory and
// whatever sources a test needs.
func launchSetup(t *testing.T, sources ...*invsource.Source) (*Dispatcher, *countingDumper) {
	t.Helper()
	ctx := context.Background()
	invs := inventory.NewMemStore()
	if err := invs.Save(ctx, &inventory.Inventory{
		ID: "inv_dyn", Name: "dynamic", Content: "[web]\nweb01\n",
	}); err != nil {
		t.Fatalf("inventories.Save() error = %v", err)
	}
	srcs := invsource.NewMemStore()
	for _, s := range sources {
		if err := srcs.Save(ctx, s); err != nil {
			t.Fatalf("sources.Save(%s) error = %v", s.ID, err)
		}
	}
	dumper := &countingDumper{}
	d := New(run.NewMemStore(), dumper, zap.NewNop(), WithNoJanitor(),
		WithInventories(invs), WithInventorySources(srcs))
	t.Cleanup(d.Close)
	return d, dumper
}

// TestRefreshOnLaunchOnlyFiresForItsOwnStaleSource pins the update-on-launch filter. It runs on the
// start path of every run, so refreshing the wrong source, or the right one on every launch when the
// operator set an interval, turns each run into an extra call to a cloud inventory API, and refusing
// to refresh at all runs the change against hosts that have moved.
func TestRefreshOnLaunchOnlyFiresForItsOwnStaleSource(t *testing.T) {
	t.Parallel()
	recent := time.Now()

	tests := []struct {
		// Name says which run and source pairing is in play.
		Name string
		// Source is the stored inventory source, or nil for an install with none.
		Source *invsource.Source
		// InventoryID is what the run targets.
		InventoryID string
		// WantDumps is how many inventory dumps the launch should trigger.
		WantDumps int
	}{{ // Test 0: A run naming no stored inventory has no source to refresh.
		Name: "a run with no stored inventory",
		Source: &invsource.Source{
			ID: "src_1", InventoryID: "inv_dyn", Source: "plugin.yml", UpdateOnLaunch: true,
		},
		InventoryID: "", WantDumps: 0,
	}, { // Test 1: A source for a different inventory is left alone.
		Name: "a source for another inventory",
		Source: &invsource.Source{
			ID: "src_1", InventoryID: "inv_other", Source: "plugin.yml", UpdateOnLaunch: true,
		},
		InventoryID: "inv_dyn", WantDumps: 0,
	}, { // Test 2: A source that did not opt into update-on-launch is left to its schedule.
		Name: "not opted into update on launch",
		Source: &invsource.Source{
			ID: "src_1", InventoryID: "inv_dyn", Source: "plugin.yml", UpdateOnLaunch: false,
		},
		InventoryID: "inv_dyn", WantDumps: 0,
	}, { // Test 3: An opted-in source synced inside its interval is still fresh.
		Name: "fresh inside its interval",
		Source: &invsource.Source{
			ID: "src_1", InventoryID: "inv_dyn", Source: "plugin.yml", UpdateOnLaunch: true,
			SyncIntervalSeconds: 3600, SyncedAt: &recent,
		},
		InventoryID: "inv_dyn", WantDumps: 0,
	}, { // Test 4: An opted-in source with no interval refreshes on every launch.
		Name: "opted in with no interval",
		Source: &invsource.Source{
			ID: "src_1", InventoryID: "inv_dyn", Source: "plugin.yml", UpdateOnLaunch: true,
		},
		InventoryID: "inv_dyn", WantDumps: 1,
	}, { // Test 5: An opted-in source past its interval refreshes.
		Name: "opted in and stale",
		Source: &invsource.Source{
			ID: "src_1", InventoryID: "inv_dyn", Source: "plugin.yml", UpdateOnLaunch: true,
			SyncIntervalSeconds: 1, SyncedAt: ptr(recent.Add(-time.Hour)),
		},
		InventoryID: "inv_dyn", WantDumps: 1,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var sources []*invsource.Source
			if test.Source != nil {
				test.Source.CreatedAt = time.Now()
				sources = append(sources, test.Source)
			}
			d, dumper := launchSetup(t, sources...)

			d.refreshOnLaunch(context.Background(), &run.Run{
				ID: "run_launch", Playbook: "site.yml", InventoryID: test.InventoryID,
			})

			if got := dumper.count(); got != test.WantDumps {
				t.Errorf("%d inventory dumps on launch, want %d", got, test.WantDumps)
			}
		})
	}
}

// TestRefreshOnLaunchWithTheFeatureOffDoesNothing pins the guard that comes first. The refresh runs on
// the start path of every run, including on an install that stores no dynamic sources at all, so
// reading a nil source store there would panic on every launch.
func TestRefreshOnLaunchWithTheFeatureOffDoesNothing(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithNoJanitor())
	defer d.Close()

	d.refreshOnLaunch(context.Background(), &run.Run{ID: "run_x", InventoryID: "inv_dyn"})
	d.refreshOnLaunch(context.Background(), &run.Run{ID: "run_y"})
}

// TestSyncDueSourcesRefreshesOnlyWhatIsDue pins the background sweep's selection. Each refresh runs an
// inventory plugin against somebody's cloud account, so sweeping a source that is not due multiplies
// that cost by however many sources an install holds.
func TestSyncDueSourcesRefreshesOnlyWhatIsDue(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d, dumper := launchSetup(t,
		&invsource.Source{
			ID: "src_due", InventoryID: "inv_dyn", Source: "due.yml",
			SyncIntervalSeconds: 1, SyncedAt: ptr(now.Add(-time.Hour)), CreatedAt: now,
		},
		&invsource.Source{
			ID: "src_fresh", InventoryID: "inv_dyn", Source: "fresh.yml",
			SyncIntervalSeconds: 3600, SyncedAt: &now, CreatedAt: now,
		},
		&invsource.Source{
			ID: "src_unscheduled", InventoryID: "inv_dyn", Source: "unscheduled.yml",
			CreatedAt: now,
		},
	)

	d.syncDueSources(context.Background())

	dumper.mu.Lock()
	got := append([]string(nil), dumper.sources...)
	dumper.mu.Unlock()
	if len(got) != 1 || got[0] != "due.yml" {
		t.Errorf("dumped %v, want only the source that was due: an unscheduled or freshly synced "+
			"source must not be re-dumped against a cloud API on every tick", got)
	}
}

// TestSyncDueSourcesStopsWithACanceledContext pins the sweep's behavior during a shutdown. The loop
// runs on the dispatcher's own context, so a stop mid-sweep must end quietly rather than logging an
// error per source about a store that is closing.
func TestSyncDueSourcesStopsWithACanceledContext(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d, _ := launchSetup(t, &invsource.Source{
		ID: "src_due", InventoryID: "inv_dyn", Source: "due.yml",
		SyncIntervalSeconds: 1, SyncedAt: ptr(now.Add(-time.Hour)), CreatedAt: now,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.syncDueSources(ctx)
}

// TestWithSourceSyncStartsTheScheduledLoop pins the wiring between the option and the goroutine it
// installs. The option is what makes one process drive scheduled inventory syncs, and a worker that
// started the loop by accident would re-dump every source on its own timer.
func TestWithSourceSyncStartsTheScheduledLoop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says how the dispatcher was configured.
		Name string
		// Opts are the options beyond the source store.
		Opts []Option
		// WantSyncing is whether the scheduled loop should be running.
		WantSyncing bool
	}{{ // Test 0: Without the option no scheduled sync runs.
		Name: "off by default", WantSyncing: false,
	}, { // Test 1: With the option the loop is installed.
		Name: "explicitly enabled", Opts: []Option{WithSourceSync()}, WantSyncing: true,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			opts := append([]Option{WithNoJanitor(),
				WithInventorySources(invsource.NewMemStore())}, test.Opts...)
			d := New(run.NewMemStore(), okRunner(), zap.NewNop(), opts...)
			defer d.Close()

			if d.syncSources != test.WantSyncing {
				t.Errorf("syncSources = %v, want %v", d.syncSources, test.WantSyncing)
			}
		})
	}

	// The option alone installs nothing without a source store to sweep, so a misconfiguration
	// cannot leave a loop polling a nil store.
	t.Run("test 2", func(t *testing.T) {
		t.Parallel()
		d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithNoJanitor(), WithSourceSync())
		defer d.Close()
		if d.invSources != nil {
			t.Error("a source store appeared without one being configured")
		}
	})
}

// TestRefreshSourceWithTheFeatureOffIsNotFound pins the guard on the manual refresh. An install
// without dynamic inventory configured has no source to refresh, and reporting that as not found is
// what keeps the handler from dereferencing a store that was never wired.
func TestRefreshSourceWithTheFeatureOffIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		// Name says which half of the wiring is missing.
		Name string
		// Opts configure the dispatcher.
		Opts []Option
	}{{ // Test 0: Nothing configured at all.
		Name: "no source store and no inventories",
	}, { // Test 1: Sources but no inventory store to write the result into.
		Name: "no inventory store",
		Opts: []Option{WithInventorySources(invsource.NewMemStore())},
	}, { // Test 2: Both stores but a runner that cannot dump an inventory.
		Name: "no dumper",
		Opts: []Option{WithInventorySources(invsource.NewMemStore()),
			WithInventories(inventory.NewMemStore())},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			opts := append([]Option{WithNoJanitor()}, test.Opts...)
			d := New(run.NewMemStore(), okRunner(), zap.NewNop(), opts...)
			defer d.Close()

			if _, err := d.RefreshSource(ctx, "src_1"); err == nil {
				t.Error("RefreshSource reported success on an install with no dynamic inventory " +
					"wiring, so a handler would read a nil result")
			}
		})
	}
}
