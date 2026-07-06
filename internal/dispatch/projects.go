package dispatch

import (
	"context"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

// WithProjects lets runs source their playbooks from git projects.
func WithProjects(store project.Store, syncer *project.Syncer) Option {
	return func(c *config) {
		c.projects = store
		c.syncer = syncer
	}
}

// validateProject confirms a referenced project exists before a run is accepted.
func (d *Dispatcher) validateProject(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if d.projects == nil || d.syncer == nil {
		return project.ErrNotFound
	}
	if _, err := d.projects.Get(ctx, id); err != nil {
		return fmt.Errorf("%w: %s", err, id)
	}
	return nil
}

// resolveProject syncs the run's project and rewrites the spec so the playbook and inventory
// resolve inside the checkout. It stamps the commit the run executes on.
func (d *Dispatcher) resolveProject(r *run.Run, spec *roundhouse.Spec) error {
	if r.ProjectID == "" {
		return nil
	}
	if d.projects == nil || d.syncer == nil {
		return project.ErrNotFound
	}
	p, err := d.projects.Get(context.Background(), r.ProjectID)
	if err != nil {
		return fmt.Errorf("project %s: %w", r.ProjectID, err)
	}

	sshKey := ""
	if p.CredentialID != "" {
		if d.credentials == nil || d.sealer == nil {
			return credential.ErrNoKey
		}
		c, err := d.credentials.Get(context.Background(), p.CredentialID)
		if err != nil {
			return fmt.Errorf("project credential %s: %w", p.CredentialID, err)
		}
		if sshKey, err = d.sealer.Open(c.Secret); err != nil {
			return fmt.Errorf("decrypt project credential: %w", err)
		}
	}

	dir, sha, err := d.syncer.Sync(p, sshKey)
	if err != nil {
		return fmt.Errorf("sync project %s: %w", p.Name, err)
	}

	playbook, err := project.WithinRepo(dir, r.Playbook)
	if err != nil {
		return fmt.Errorf("playbook %q: %w", r.Playbook, err)
	}
	spec.Playbook = playbook
	if r.Inventory != "" {
		inventory, err := project.WithinRepo(dir, r.Inventory)
		if err != nil {
			return fmt.Errorf("inventory %q: %w", r.Inventory, err)
		}
		spec.Inventory = inventory
	}
	spec.Dir = dir
	r.CommitSHA = sha
	return nil
}
