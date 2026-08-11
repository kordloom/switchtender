package dispatch

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// WithProjects lets runs source their playbooks from git projects.
func WithProjects(store project.Store, syncer *project.Syncer) Option {
	return func(c *config) {
		c.projects = store
		c.syncer = syncer
	}
}

// WithDefaultImage sets the fallback execution image applied when a run, its template, and its
// project pin none. Empty leaves an unpinned run on the host.
func WithDefaultImage(image string) Option {
	return func(c *config) { c.defaultImage = image }
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
// resolve inside an isolated per-run checkout. It stamps the commit the run executes on and returns
// a cleanup that removes the checkout, which the caller defers so the copy does not outlive the run.
// The returned cleanup is always safe to call, including on the error paths.
func (d *Dispatcher) resolveProject(r *run.Run, spec *roundhouse.Spec) (cleanup func(), err error) {
	cleanup = func() {}
	if r.ProjectID == "" {
		return cleanup, nil
	}
	if d.projects == nil || d.syncer == nil {
		return cleanup, project.ErrNotFound
	}
	p, err := d.projects.Get(context.Background(), r.ProjectID)
	if err != nil {
		return cleanup, fmt.Errorf("project %s: %w", r.ProjectID, err)
	}

	sshKey := ""
	if p.CredentialID != "" {
		if d.credentials == nil || d.sealer == nil {
			return cleanup, credential.ErrNoKey
		}
		c, err := d.credentials.Get(context.Background(), p.CredentialID)
		if err != nil {
			return cleanup, fmt.Errorf("project credential %s: %w", p.CredentialID, err)
		}
		if sshKey, err = d.sealer.Open(c.Secret); err != nil {
			return cleanup, fmt.Errorf("decrypt project credential: %w", err)
		}
	}

	wt, err := d.syncer.Sync(p, sshKey)
	if err != nil {
		return cleanup, fmt.Errorf("sync project %s: %w", p.Name, err)
	}
	// From here a checkout exists on disk, so every return removes it. A caller that gets a non-nil
	// cleanup and an error still calls cleanup, so this cannot leak the copy on a later failure.
	cleanup = wt.Cleanup
	spec.Env = append(spec.Env, wt.GalaxyEnv...)

	playbook, err := project.WithinRepo(wt.Dir, r.Playbook)
	if err != nil {
		return cleanup, fmt.Errorf("playbook %q: %w", r.Playbook, err)
	}
	spec.Playbook = playbook
	if r.Inventory != "" {
		inventory, err := project.WithinRepo(wt.Dir, r.Inventory)
		if err != nil {
			return cleanup, fmt.Errorf("inventory %q: %w", r.Inventory, err)
		}
		spec.Inventory = inventory
	}
	spec.Dir = wt.Dir
	r.CommitSHA = wt.SHA

	// The run's own image, from the request or its template, outranks the project's.
	if p.Image != "" && spec.Image == "" {
		spec.Image = p.Image
		if err := d.resolvePullCredential(p.PullCredentialID, spec); err != nil {
			return cleanup, err
		}
	}
	return cleanup, nil
}

// applyDefaultImage falls back to the server-wide default execution image when a run, its template,
// and its project pin none, so one default environment can apply without pinning an image on every
// project. It never overrides an image already resolved.
func (d *Dispatcher) applyDefaultImage(spec *roundhouse.Spec) {
	if spec.Image == "" {
		spec.Image = d.defaultImage
	}
}

// resolvePullCredential decrypts the named registry credential, when set, onto the spec so the
// container runner can pull a private execution environment image.
func (d *Dispatcher) resolvePullCredential(id string, spec *roundhouse.Spec) error {
	if id == "" {
		return nil
	}
	if d.credentials == nil || d.sealer == nil {
		return credential.ErrNoKey
	}
	c, err := d.credentials.Get(context.Background(), id)
	if err != nil {
		return fmt.Errorf("pull credential %s: %w", id, err)
	}
	plain, err := d.sealer.Open(c.Secret)
	if err != nil {
		return fmt.Errorf("decrypt pull credential: %w", err)
	}
	spec.RegistryUsername, spec.RegistryPassword = credential.RegistryLogin(plain)
	return nil
}

// registrySecrets returns the registry pull login values that must be masked from run output. The
// pull credential resolves onto the spec outside materializeCredentials, so its password would not
// otherwise reach the masker, unlike every other credential.
func registrySecrets(spec *roundhouse.Spec) []string {
	var out []string
	if spec.RegistryPassword != "" {
		out = append(out, spec.RegistryPassword)
	}
	if spec.RegistryUsername != "" {
		out = append(out, spec.RegistryUsername)
	}
	return out
}
