package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// factSentinel is a value only a fact document carries, so finding it anywhere evidential proves
// facts leaked there.
const factSentinel = "FACT-SENTINEL-7731"

// factPlay stands in for ansible-playbook's jsonfile cache plugin as ansible-core 2.18 and earlier
// lay it out, one file per host named after the host: it records which hosts' facts a run was
// served, then writes and removes cache files the way a play that gathers and clears does. The
// schema-prefixed files later releases read are beside them and ignored here, as 2.18 ignores
// them.
type factPlay struct {
	// mu guards every field, since the runner executes on a dispatcher goroutine.
	mu sync.Mutex
	// write is the files the next run writes, host to document.
	write map[string]string
	// remove is the hosts whose files the next run deletes.
	remove []string
	// served is what the last run found in its cache directory, host to document.
	served map[string]string
	// dir is the cache directory the last run was given, empty when it was given none.
	dir string
}

// next sets what the next run writes and removes.
func (p *factPlay) next(write map[string]string, remove ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.write, p.remove = write, remove
}

// last returns what the last run was served and the directory it was given.
func (p *factPlay) last() (map[string]string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.served, p.dir
}

// runner returns the fake executor.
func (p *factPlay) runner() roundhouse.Runner {
	return roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.dir, p.served = spec.FactCacheDir, map[string]string{}
		if spec.FactCacheDir == "" {
			return roundhouse.Result{}, nil
		}
		files, err := os.ReadDir(spec.FactCacheDir)
		if err != nil {
			return roundhouse.Result{ExitCode: 1}, err
		}
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "s1_") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(spec.FactCacheDir, f.Name()))
			if err != nil {
				return roundhouse.Result{ExitCode: 1}, err
			}
			p.served[f.Name()] = string(data)
		}
		for host, doc := range p.write {
			if err := os.WriteFile(filepath.Join(spec.FactCacheDir, host), []byte(doc), 0o600); err != nil {
				return roundhouse.Result{ExitCode: 1}, err
			}
		}
		for _, host := range p.remove {
			_ = os.Remove(filepath.Join(spec.FactCacheDir, host))
		}
		return roundhouse.Result{}, nil
	})
}

// hostsOf returns the sorted keys of a served cache.
func hostsOf(served map[string]string) []string {
	out := make([]string, 0, len(served))
	for h := range served {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// TestFactCacheRoundTrip drives runs through the dispatcher the way AWX's fact cache behaves: the
// facts a run gathers are stored per inventory host, the next run is served them, a host older than
// the template's timeout is not served, a host a play clears loses its facts, a host the inventory
// does not name is not kept, and a run with the cache off is given no directory at all. Throughout,
// no fact reaches the audit chain, the outcome record, or the run itself.
func TestFactCacheRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	facts := factcache.NewMemStore()
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: "[web]\nweb01\nweb02\n", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	play := &factPlay{}
	d := New(store, play.runner(), nil, WithAudits(audits), WithNoJanitor(),
		WithInventories(inventories), WithFactCache(facts))
	defer d.Close()
	launch := func(opts ...run.SubmitOption) *run.Run {
		t.Helper()
		r, err := d.Submit(ctx, "site.yml", "", append(opts, run.WithInventory("inv_1"))...)
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		return waitTerminal(t, store, r.ID)
	}
	cached := func() []string {
		t.Helper()
		list, err := facts.List(ctx, "inv_1", false)
		if err != nil {
			t.Fatal(err)
		}
		var hosts []string
		for _, e := range list {
			hosts = append(hosts, e.Host)
		}
		return hosts
	}

	// The first run is served nothing, gathers web01, and writes a file for a host the inventory
	// does not hold.
	play.next(map[string]string{
		"web01": "{\n    \"ansible_distribution\": \"Debian\",\n    \"token\": \"" +
			factSentinel + "\"\n}",
		"stranger": `{"ansible_distribution":"planted"}`,
	})
	first := launch(run.WithFactCache(true, 0))
	served, _ := play.last()
	if len(served) != 0 {
		t.Errorf("the first run was served %v, want an empty cache", hostsOf(served))
	}
	if diff := cmp.Diff([]string{"web01"}, cached()); diff != "" {
		t.Errorf("cached hosts after the first run mismatch (-want +got):\n%s", diff)
	}
	got, err := facts.Facts(ctx, "inv_1", "web01")
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != first.ID || !strings.Contains(string(got.Facts), "Debian") {
		t.Errorf("cached web01 = %s from %s, want the first run's gather", got.Facts, got.RunID)
	}
	if !strings.Contains(first.Warning, "did not keep 1") {
		t.Errorf("first run warning = %q, want it to say a fact file was not kept", first.Warning)
	}

	// web02 was gathered two hours ago. With a one hour timeout it is too old to serve, so the run
	// gathers it again. The run clears web01, as meta: clear_facts would.
	if err := facts.SaveFacts(ctx, []factcache.Entry{{InventoryID: "inv_1", Host: "web02",
		Facts: json.RawMessage(`{"ansible_distribution":"Ubuntu"}`), RunID: first.ID,
		ModifiedAt: time.Now().Add(-2 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	play.next(nil, "web01")
	launch(run.WithFactCache(true, 3600))
	served, _ = play.last()
	if diff := cmp.Diff([]string{"web01"}, hostsOf(served)); diff != "" {
		t.Errorf("served hosts under a one hour timeout mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(served["web01"], factSentinel) {
		t.Errorf("web01 was served %q, want the facts the first run gathered", served["web01"])
	}
	if diff := cmp.Diff([]string{"web02"}, cached()); diff != "" {
		t.Errorf("cached hosts after a clear mismatch (-want +got):\n%s", diff)
	}

	// With no timeout the old web02 facts are served.
	play.next(nil)
	launch(run.WithFactCache(true, 0))
	served, _ = play.last()
	if diff := cmp.Diff([]string{"web02"}, hostsOf(served)); diff != "" {
		t.Errorf("served hosts with no timeout mismatch (-want +got):\n%s", diff)
	}

	// A run with the cache off is given no directory and changes nothing.
	play.next(map[string]string{"web01": `{"x":1}`})
	off := launch()
	if _, dir := play.last(); dir != "" {
		t.Errorf("a run with the cache off was given %q", dir)
	}
	if diff := cmp.Diff([]string{"web02"}, cached()); diff != "" {
		t.Errorf("a run with the cache off changed the cache (-want +got):\n%s", diff)
	}
	if strings.Contains(off.Warning, "fact cache") {
		t.Errorf("a run with the cache off warns about it: %q", off.Warning)
	}
	assertNoFactsInEvidence(t, ctx, store, audits)
}

// assertNoFactsInEvidence fails when the fact sentinel appears in the audit chain, an outcome
// record, a run, its events, or its log.
func assertNoFactsInEvidence(t *testing.T, ctx context.Context, store run.Store, audits audit.Store) {
	t.Helper()
	entries, err := audits.Chain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the chain is empty, so this check proves nothing")
	}
	blob, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), factSentinel) {
		t.Errorf("a cached fact reached the audit chain")
	}
	runs, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		body, err := outcome.Body(ctx, store, r)
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := json.Marshal(r)
		events, _ := store.Events(ctx, r.ID)
		evs, _ := json.Marshal(events)
		log, _ := store.Log(ctx, r.ID)
		for name, b := range map[string][]byte{"outcome": body, "run": rec, "events": evs, "log": log} {
			if strings.Contains(string(b), factSentinel) {
				t.Errorf("a cached fact reached run %s's %s", r.ID, name)
			}
		}
	}
}

// TestFactCacheLivesInALockedRunDirectory pins that a run's fact cache sits inside a locked run
// directory under the run files root, the same place every credential file goes, so a process
// killed mid-run leaves the facts to the crash sweep, and that the directory is gone once the run
// ends.
func TestFactCacheLivesInALockedRunDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: "[web]\nweb01\n", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// seen is what the executor observed about its cache directory while the run held it.
	type seen struct {
		// Parent is the directory two levels up from the cache directory.
		Parent string
		// Locked reports whether the enclosing run directory carried its lock file.
		Locked bool
	}
	got := make(chan seen, 1)
	runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		owner := filepath.Dir(spec.FactCacheDir)
		_, err := os.Stat(filepath.Join(owner, ".lock"))
		got <- seen{Parent: filepath.Dir(owner), Locked: err == nil}
		return roundhouse.Result{}, nil
	})
	d := New(store, runner, nil, WithNoJanitor(), WithInventories(inventories),
		WithFactCache(factcache.NewMemStore()), WithRunFilesRoot(root))
	defer d.Close()
	r, err := d.Submit(ctx, "site.yml", "", run.WithInventory("inv_1"), run.WithFactCache(true, 0))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, r.ID)
	if diff := cmp.Diff(seen{Parent: root, Locked: true}, <-got); diff != "" {
		t.Errorf("fact cache placement mismatch (-want +got):\n%s", diff)
	}
	deadline := time.Now().Add(waitBudget)
	for {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		// The sweep's lock file sits in the root beside the run directories and is not one.
		var left []string
		for _, e := range entries {
			if e.IsDir() {
				left = append(left, e.Name())
			}
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a fact cache directory outlived the run: %s", left[0])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFactCacheUnavailableSaysSo pins that a run asking for the fact cache on an executor that has
// none, a relay worker for one, says so on the run rather than quietly gathering everything again.
func TestFactCacheUnavailableSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: "web01\n", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	play := &factPlay{}
	d := New(store, play.runner(), nil, WithNoJanitor(), WithInventories(inventories))
	defer d.Close()
	r, err := d.Submit(ctx, "site.yml", "", run.WithInventory("inv_1"), run.WithFactCache(true, 0))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	done := waitTerminal(t, store, r.ID)
	if !strings.Contains(done.Warning, "fact cache is not reachable from this executor") {
		t.Errorf("warning = %q, want it to say the cache was unavailable", done.Warning)
	}
	if _, dir := play.last(); dir != "" {
		t.Errorf("the run was given a cache directory %q with no store behind it", dir)
	}
}

// TestPlainLimitHosts pins when a limit names its hosts outright, which is when only those hosts'
// facts are read rather than the whole inventory's.
func TestPlainLimitHosts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Limit     string
		WantHosts []string
		WantPlain bool
	}{
		{Limit: "web01", WantHosts: []string{"web01"}, WantPlain: true}, // Test 0: One host.
		// Test 1: A list.
		{Limit: "web01, db.example.com", WantHosts: []string{"web01", "db.example.com"}, WantPlain: true},
		{Limit: ""},          // Test 2: No limit reads everything.
		{Limit: "web*"},      // Test 3: A wildcard.
		{Limit: "web:&prod"}, // Test 4: An intersection.
		{Limit: "!db01"},     // Test 5: An exclusion.
		{Limit: "@retry"},    // Test 6: A file reference.
		{Limit: "web[1:3]"},  // Test 7: A range.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			hosts, plain := plainLimitHosts(test.Limit)
			if plain != test.WantPlain {
				t.Errorf("plainLimitHosts(%q) plain = %v, want %v", test.Limit, plain, test.WantPlain)
			}
			if diff := cmp.Diff(test.WantHosts, hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hosts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
