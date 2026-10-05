package dispatch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
)

// WithFactCache lets a run whose template turns the fact cache on read the facts earlier runs
// gathered for its inventory's hosts and keep the facts it gathers. A process without it, such as
// a worker that reaches the control node over the relay and holds no database, runs those templates
// without the cache and says so on the run.
func WithFactCache(store factcache.Store) Option {
	return func(c *config) { c.factCache = store }
}

// factCacheRun is one run's fact cache: the directory Ansible reads and writes, and the inventory
// whose hosts it caches.
type factCacheRun struct {
	// owner is the locked run directory the cache directory lives in, so a process killed mid-run
	// leaves the facts to the same crash sweep as every credential file.
	owner *runfiles.Dir
	// dir is the private cache directory written before the run.
	dir *factcache.Dir
	// inventoryID is the stored inventory the cached hosts belong to.
	inventoryID string
	// prepared is when the cached facts were written for Ansible, before the run began, so no fact
	// the run collects was gathered earlier.
	prepared time.Time
}

// remove deletes the run's cache directory. It is safe on a nil receiver, which is a run that does
// not use the cache.
func (f *factCacheRun) remove() {
	if f != nil {
		f.dir.Remove()
		_ = f.owner.Remove()
	}
}

// prepareFactCache writes the cached facts for r's inventory into a private directory and points
// the spec at it, following the AWX model: Ansible's jsonfile plugin reads a host's file instead of
// gathering, and rewrites it when the play gathers again. It returns nil when r does not use the
// cache or cannot, recording why on the run when the template asked for it, because a run that
// silently gathered every fact again would look like a cache that works.
//
// The cache never stops a run. Facts are an optimization a play falls back from by gathering, so a
// read that fails costs time rather than the change the run was asked to make.
func (d *Dispatcher) prepareFactCache(ctx context.Context, r *run.Run, spec *roundhouse.Spec) *factCacheRun {
	if !r.UseFactCache {
		return nil
	}
	switch {
	case run.NormalizeTool(r.Tool) != run.ToolAnsible:
		addWarning(r, "the fact cache is an Ansible feature, so this "+run.NormalizeTool(r.Tool)+
			" run did not use it")
		return nil
	case r.InventoryID == "":
		addWarning(r, "the fact cache keeps facts per stored inventory host, and this run names no "+
			"stored inventory, so it did not use it")
		return nil
	case d.factCache == nil:
		addWarning(r, "the fact cache is not reachable from this executor, so this run gathered its "+
			"facts without it and kept none")
		return nil
	}
	// The cache directory sits inside a locked run directory under the same root as the run's
	// credential files, so the facts get the same 0700 directory, the same removal, and the same
	// sweep after a crash.
	owner, err := runfiles.Create(d.runFiles(), r.ID+"-facts")
	if err != nil {
		d.log.Warn("dispatch: fact cache unavailable: "+err.Error(), zap.String("run_id", r.ID))
		addWarning(r, "the fact cache directory could not be created, so this run did not use it")
		return nil
	}
	dir, err := factcache.NewDir(owner.Path())
	if err != nil {
		_ = owner.Remove()
		d.log.Warn("dispatch: fact cache unavailable: "+err.Error(), zap.String("run_id", r.ID))
		addWarning(r, "the fact cache directory could not be created, so this run did not use it")
		return nil
	}
	entries, err := d.cachedFacts(ctx, r, spec.Inventory)
	if err != nil {
		d.log.Warn("dispatch: read cached facts: "+err.Error(), zap.String("run_id", r.ID))
		addWarning(r, "the cached facts could not be read, so this run gathered without them")
	}
	timeout := time.Duration(r.FactCacheTimeout) * time.Second
	prepared := d.now()
	if _, err := dir.Write(entries, prepared, timeout); err != nil {
		d.log.Warn("dispatch: write cached facts: "+err.Error(), zap.String("run_id", r.ID))
		addWarning(r, "the cached facts could not be written for Ansible, so this run gathered "+
			"without them")
	}
	spec.FactCacheDir = dir.Path
	return &factCacheRun{owner: owner, dir: dir, inventoryID: r.InventoryID, prepared: prepared}
}

// cachedFacts reads the facts the run's inventory holds. When the run's limit names only hosts the
// inventory at inventoryPath holds, as a provisioning callback's single host does, only those are
// read, so one host booting in a large fleet does not pull every host's facts off disk. A name that
// is not a host is a group, which may reach any host, so then every host's facts are read.
func (d *Dispatcher) cachedFacts(ctx context.Context, r *run.Run,
	inventoryPath string) ([]factcache.Entry, error) {
	if hosts, ok := plainLimitHosts(r.Limit); ok && inventoryHoldsHosts(inventoryPath, hosts) {
		out := make([]factcache.Entry, 0, len(hosts))
		for _, h := range hosts {
			e, err := d.factCache.Facts(ctx, r.InventoryID, h)
			if err != nil {
				continue
			}
			out = append(out, *e)
		}
		return out, nil
	}
	return d.factCache.List(ctx, r.InventoryID, true)
}

// inventoryHoldsHosts reports whether every name is a host of the inventory file at path. A file
// that cannot be read as a host list answers false, so the caller reads every host's facts rather
// than too few.
func inventoryHoldsHosts(path string, names []string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	hosts, err := inventory.Hosts(string(data))
	if err != nil {
		return false
	}
	held := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		held[h.Name] = true
	}
	for _, n := range names {
		if !held[n] {
			return false
		}
	}
	return true
}

// plainLimitHosts returns the host names a limit lists outright, or false when the limit is empty
// or uses anything Ansible reads as a pattern: a wildcard, a range, a regular expression, a group
// intersection or exclusion, or a file reference.
func plainLimitHosts(limit string) ([]string, bool) {
	limit = strings.TrimSpace(limit)
	if limit == "" || strings.ContainsAny(limit, "*?[]!&~@:") {
		return nil, false
	}
	var out []string
	for _, h := range strings.Split(limit, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out, len(out) > 0
}

// collectFactCache stores what the run changed in its cache directory: the facts each host it
// gathered now holds, and the removal of any host a play cleared. Only hosts the run's inventory
// names, or that the run's recap shows it reached, are kept, so a play cannot plant facts for a
// host the inventory does not hold. It runs whatever the run's outcome, as AWX does, since a play
// that failed on one host still gathered the others.
func (d *Dispatcher) collectFactCache(r *run.Run, fc *factCacheRun, inventoryPath string,
	fold *run.SummaryFold) {
	if fc == nil {
		return
	}
	allowed := map[string]bool{}
	if data, err := os.ReadFile(inventoryPath); err == nil {
		if hosts, herr := inventory.Hosts(string(data)); herr == nil {
			for _, h := range hosts {
				allowed[h.Name] = true
			}
		}
	}
	for _, s := range fold.HostSummaries() {
		allowed[s.Host] = true
	}
	got, err := fc.dir.Collect(func(host string) bool { return allowed[host] })
	if err != nil {
		d.log.Warn("dispatch: collect cached facts: "+err.Error(), zap.String("run_id", r.ID))
		addWarning(r, "the facts this run gathered could not be read back, so the fact cache "+
			"was not updated")
		return
	}
	if len(got.Skipped) > 0 {
		// Names and reasons only. A skipped document is never logged, since facts are exactly the
		// kind of value the log must not carry.
		d.log.Warn("dispatch: fact cache files not kept", zap.String("run_id", r.ID),
			zap.Strings("skipped", got.Skipped))
		addWarning(r, fmt.Sprintf("the fact cache did not keep %d of the fact files this run "+
			"wrote: a host outside the inventory, a document larger than the limit, or one that is "+
			"not a JSON object", len(got.Skipped)))
	}
	// Each host is stamped with when its facts were gathered rather than when the run ended. The
	// timeout is measured from that stamp, so a run that gathered first and then ran for hours
	// handed the next run facts as old as itself while calling them new, and the store keeps the
	// newer of two gathers by it, so a run that gathered earlier and finished later wrote its older
	// facts over newer ones.
	now := d.now()
	for i := range got.Updated {
		got.Updated[i].InventoryID = fc.inventoryID
		got.Updated[i].RunID = r.ID
		got.Updated[i].ModifiedAt = gatheredAt(got.Updated[i].ModifiedAt, fc.prepared, now)
	}
	ctx := context.Background()
	if len(got.Updated) > 0 {
		if err := withRetries(func() error {
			return d.factCache.SaveFacts(ctx, got.Updated)
		}); err != nil {
			d.log.Error("dispatch: save cached facts: "+err.Error(), zap.String("run_id", r.ID))
			addWarning(r, "the facts this run gathered could not be saved to the fact cache")
		}
	}
	if len(got.Cleared) > 0 {
		if _, err := d.factCache.Clear(ctx, fc.inventoryID, got.Cleared...); err != nil {
			d.log.Error("dispatch: clear cached facts: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
}

// gatheredAt returns when a host's facts were gathered: wrote, the time Ansible wrote its cache
// file, held inside the run's own window from prepared, when the run's cached facts were written,
// to now. The file was written during the run, so a clock reading that places it outside that
// window, from a play that set the file's time or a clock that stepped, is held to the nearer edge,
// and a file time ahead of now can never make facts look fresher than the run that gathered them.
func gatheredAt(wrote, prepared, now time.Time) time.Time {
	switch {
	case wrote.IsZero() || wrote.After(now):
		return now
	case wrote.Before(prepared):
		return prepared
	}
	return wrote
}
