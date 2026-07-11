package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/project"
)

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
// run arbitrary code or reach outside its intended location as the executor.
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
