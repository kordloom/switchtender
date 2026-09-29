package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// WithSourceSync enables a background loop that refreshes dynamic inventory sources on their
// configured interval. Enable it on the server so one process drives scheduled syncs; workers rely on
// update-on-launch instead.
func WithSourceSync() Option {
	return func(c *config) { c.syncSources = true }
}

// WithInventorySources lets the dispatcher refresh dynamic inventory sources into stored
// inventories.
func WithInventorySources(store invsource.Store) Option {
	return func(c *config) { c.invSources = store }
}

// RefreshSource runs the source's inventory plugin or script and writes the resulting hosts into
// the stored inventory the source maintains. The source records when it last synced and any
// failure, so a broken source is visible rather than silently stale.
func (d *Dispatcher) RefreshSource(ctx context.Context, id string) (*invsource.Source, error) {
	if d.invSources == nil || d.inventories == nil || d.dumper == nil {
		return nil, invsource.ErrNotFound
	}
	src, err := d.invSources.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	data, refreshErr := d.dumpSource(ctx, src)
	if refreshErr != nil {
		src.LastError = refreshErr.Error()
		_ = d.invSources.Save(ctx, src)
		return src, refreshErr
	}

	inv, err := d.inventories.Get(ctx, src.InventoryID)
	if err != nil {
		return nil, fmt.Errorf("source inventory %s: %w", src.InventoryID, err)
	}
	static, err := staticFromDump(data)
	if err != nil {
		src.LastError = "parse inventory dump: " + err.Error()
		_ = d.invSources.Save(ctx, src)
		return src, fmt.Errorf("parse inventory dump: %w", err)
	}
	inv.Content = string(static)
	if err := d.inventories.Save(ctx, inv); err != nil {
		return nil, fmt.Errorf("write source inventory: %w", err)
	}

	now := time.Now()
	src.SyncedAt = &now
	src.LastError = ""
	if err := d.invSources.Save(ctx, src); err != nil {
		return nil, fmt.Errorf("save source: %w", err)
	}
	return src, nil
}

// dumpSource resolves the source's credential and project and renders it to inventory JSON.
func (d *Dispatcher) dumpSource(ctx context.Context, src *invsource.Source) ([]byte, error) {
	var env []string
	if src.CredentialID != "" {
		if d.credentials == nil || d.sealer == nil {
			return nil, credential.ErrNoKey
		}
		c, err := d.credentials.Get(ctx, src.CredentialID)
		if err != nil {
			return nil, fmt.Errorf("source credential %s: %w", src.CredentialID, err)
		}
		plain, err := d.sealer.Open(c.Secret)
		if err != nil {
			return nil, fmt.Errorf("decrypt source credential: %w", err)
		}
		if c.Kind == credential.KindEnv {
			env = credential.EnvLines(plain)
		}
	}

	sourcePath := src.Source
	if src.ProjectID != "" {
		if d.projects == nil || d.syncer == nil {
			return nil, project.ErrNotFound
		}
		p, err := d.projects.Get(ctx, src.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("source project %s: %w", src.ProjectID, err)
		}
		dir, _, _, err := d.syncer.Sync(p, "")
		if err != nil {
			return nil, fmt.Errorf("sync source project: %w", err)
		}
		if sourcePath, err = project.WithinRepo(dir, src.Source); err != nil {
			return nil, fmt.Errorf("source path %q: %w", src.Source, err)
		}
	} else if err := validateBareSource(src.Source); err != nil {
		return nil, err
	}
	return d.dumper.Dump(ctx, sourcePath, env)
}

// validateBareSource guards a non-project inventory source path. A bare source has no project repo
// to contain it, so it is a raw host path handed to ansible-inventory, which executes it when it is
// an executable file. It rejects directory traversal and executable files so a stored source cannot
// run arbitrary code as the executor.
//
// An absolute path is deliberately allowed. Pointing at a file on the executor is what a bare source
// is for, and writing one is an admin operation on a server where an admin can already submit a
// playbook that reads any file. Refusing absolute paths would remove the feature without removing
// the reach, so the guard here is against execution rather than against location.
func validateBareSource(source string) error {
	if source == "" {
		return fmt.Errorf("%w: empty path", invsource.ErrInvalidSource)
	}
	for _, seg := range strings.Split(filepath.ToSlash(source), "/") {
		if seg == ".." {
			return fmt.Errorf("%w: %q traverses directories", invsource.ErrInvalidSource, source)
		}
	}
	info, err := os.Stat(source)
	if err != nil {
		// A missing or unreadable path is not an execution surface; let ansible-inventory report it.
		return nil //nolint:nilerr // Absence is handled downstream, not a validation failure.
	}
	if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return fmt.Errorf("%w: %q is executable", invsource.ErrInvalidSource, source)
	}
	return nil
}

// sourceSyncInterval is how often the sync loop checks which sources are due, bounding how late a
// scheduled refresh can run past its interval.
const sourceSyncInterval = 30 * time.Second

// sourceSyncLoop refreshes dynamic inventory sources on their configured intervals until the
// dispatcher closes.
func (d *Dispatcher) sourceSyncLoop() {
	defer d.wg.Done()
	ticker := time.NewTicker(sourceSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.syncDueSources(d.ctx)
		}
	}
}

// syncDueSources refreshes every source whose scheduled interval has elapsed since its last sync.
func (d *Dispatcher) syncDueSources(ctx context.Context) {
	srcs, err := d.invSources.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("dispatch: list sources for sync: " + err.Error())
		}
		return
	}
	now := time.Now()
	for _, src := range srcs {
		if !sourceDue(src, now) {
			continue
		}
		if _, err := d.RefreshSource(ctx, src.ID); err != nil && ctx.Err() == nil {
			d.log.Warn("dispatch: scheduled source refresh failed: "+err.Error(),
				zap.String("source", src.ID))
		}
	}
}

// sourceDue reports whether a source with a positive sync interval is due for a scheduled refresh:
// never synced, or last synced longer ago than its interval.
func sourceDue(src *invsource.Source, now time.Time) bool {
	if src.SyncIntervalSeconds <= 0 {
		return false
	}
	if src.SyncedAt == nil {
		return true
	}
	return now.Sub(*src.SyncedAt) >= time.Duration(src.SyncIntervalSeconds)*time.Second
}

// refreshOnLaunch refreshes the dynamic inventory source backing a run's inventory when the source
// opts into update-on-launch and its data is stale, so the run sees current hosts. It is best effort:
// a refresh failure is logged and the run proceeds with the last good inventory.
func (d *Dispatcher) refreshOnLaunch(ctx context.Context, r *run.Run) {
	if d.invSources == nil || r.InventoryID == "" {
		return
	}
	srcs, err := d.invSources.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("dispatch: list sources for launch refresh: " + err.Error())
		}
		return
	}
	now := time.Now()
	for _, src := range srcs {
		if src.InventoryID != r.InventoryID || !src.UpdateOnLaunch {
			continue
		}
		if !launchStale(src, now) {
			return
		}
		if _, err := d.RefreshSource(ctx, src.ID); err != nil && ctx.Err() == nil {
			d.log.Warn("dispatch: update-on-launch refresh failed: "+err.Error(),
				zap.String("source", src.ID))
		}
		return
	}
}

// launchStale reports whether an update-on-launch source is stale enough to refresh before a run: a
// zero interval always refreshes, otherwise it refreshes once its interval has elapsed.
func launchStale(src *invsource.Source, now time.Time) bool {
	if src.SyncIntervalSeconds <= 0 || src.SyncedAt == nil {
		return true
	}
	return now.Sub(*src.SyncedAt) >= time.Duration(src.SyncIntervalSeconds)*time.Second
}
