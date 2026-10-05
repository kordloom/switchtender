package dispatch

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/policy"
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
func (d *Dispatcher) resolveProject(ctx context.Context, r *run.Run, spec *roundhouse.Spec) (cleanup func(), err error) {
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

	sshKey, release, err := d.projectKey(ctx, p)
	defer release()
	if err != nil {
		return cleanup, err
	}

	// A run naming a ref executes the commit that ref holds rather than the branch tip. That is how a
	// review plan runs a pull request's head, which sits on no branch the project tracks. The pin it
	// always carries is enforced below exactly as for any pinned run.
	var wt *project.Worktree
	if r.GitRef != "" {
		wt, err = d.syncer.SyncRef(p, sshKey, r.GitRef)
	} else {
		wt, err = d.syncer.Sync(p, sshKey)
	}
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
	// A stored inventory chosen at launch wins over a path inside the checkout.
	//
	// materializeInventory runs before this and writes the stored inventory to a file, setting
	// spec.Inventory to it. Rewriting that from the project discarded it silently: the run executed
	// against the repository's inventory file while its own record, the run detail page and the
	// receipt all named the stored inventory the operator picked. Two answers to "which hosts did
	// this run touch", and the durable one was wrong.
	if r.Inventory != "" && r.InventoryID == "" {
		inventory, err := project.WithinRepo(wt.Dir, r.Inventory)
		if err != nil {
			return cleanup, fmt.Errorf("inventory %q: %w", r.Inventory, err)
		}
		spec.Inventory = inventory
	}
	spec.Dir = wt.Dir
	// Recording which commit was checked out and enforcing the pin are one step, deliberately: a run
	// pinned to a commit executes that commit or nothing, and the moment the commit becomes known is
	// the moment that can be decided.
	if err := stampCommit(r, wt.SHA); err != nil {
		return cleanup, err
	}

	// The project's image is not adopted here. It was pinned onto the run, with its pull credential,
	// when the run was submitted, so the image an approval covers is the image the run executes in,
	// and a project whose image changed since cannot move an approved run into another container.
	return cleanup, nil
}

// pinHeldRunCommit stamps the commit a held git-backed run is allowed to execute, at the moment it
// is held.
//
// Approval binds the spec digest, but for a run drawn from a git project the spec names a branch,
// and the branch is a moving pointer: between the approver's yes and the worker's claim, HEAD can
// advance, so the release executed code nobody judged. The plan gate already pins its proposals;
// blanket holds did not. Pinning at hold time makes the approver's decision mean the commit that
// was current when the request was made, and execution already refuses a pin that no longer
// matches. A sync failure downgrades to an unpinned hold, which is exactly the old behavior, but
// says so on the run so the approver can see the guarantee is absent rather than assume it.
func (d *Dispatcher) pinHeldRunCommit(r *run.Run) {
	if r.Status != run.StatusPendingApproval || r.ProjectID == "" || r.PinnedCommit != "" ||
		r.GitRef != "" {
		return
	}
	if d.projects == nil || d.syncer == nil {
		return
	}
	fail := func(err error) {
		d.log.Warn("dispatch: pin held run commit: " + err.Error())
		r.Warning = strings.TrimSpace(r.Warning + " The commit could not be pinned when this run " +
			"was held, so approval releases whatever the branch holds at execution time.")
	}
	p, err := d.projects.Get(context.Background(), r.ProjectID)
	if err != nil {
		fail(err)
		return
	}
	// Resolved and unlocked the same way the sync during execution does. Pinning the commit runs the
	// identical git operation, so a credential that works for one and not the other would hold a run
	// at approval time for a reason that disappears by execution.
	sshKey, release, err := d.projectKey(context.Background(), p)
	defer release()
	if err != nil {
		fail(err)
		return
	}
	wt, err := d.syncer.Sync(p, sshKey)
	if err != nil {
		fail(err)
		return
	}
	defer wt.Cleanup()
	r.PinnedCommit = wt.SHA
}

// projectKey returns the SSH key the project's syncs authenticate with, empty for a remote that
// needs none, and a release for the credential lease behind it, which the caller defers whether or
// not an error came back. Execution, a held run's pin, and the gate's fetch all sync through it, so
// a credential that works for one of them works for all three.
//
// It takes the same two steps run materialization applies to this kind: resolve through the
// credential's source, then unlock the key. Handing the stored value straight to the SSH parser
// meant a passphrase-protected key arrived as the JSON wrapper it is stored in, and every sync of
// that project failed with an error naming the key rather than the omission.
func (d *Dispatcher) projectKey(ctx context.Context, p *project.Project) (string, func(), error) {
	release := func() {}
	if p.CredentialID == "" {
		return "", release, nil
	}
	if d.credentials == nil || d.sealer == nil {
		return "", release, credential.ErrNoKey
	}
	_, plain, lease, err := d.openCredential(ctx, p.CredentialID)
	if err != nil {
		return "", release, fmt.Errorf("project credential %s: %w", p.CredentialID, err)
	}
	release = func() { d.revokeLease(lease) }
	unlocked, _, err := sshKeyFrom(plain)
	if err != nil {
		return "", release, fmt.Errorf("project credential %s: %w", p.CredentialID, err)
	}
	return unlocked, release, nil
}

// refreshForGate brings the checkouts a submission draws its playbooks and configurations from up
// to date, once, before the gate first grades them. Each submission path calls it from its first
// rule check, so every later check in the same submission reads the commit it fetched.
//
// The gate reads a project run's playbook from the project's checkout, and the checkout holds
// whatever the last sync fetched. Graded from that alone, a destructive role pushed a minute ago
// and launched straight away was graded on the commit before it, and ran past a rule written to
// hold exactly that change. Fetching first makes the grade describe the commit the run is about to
// execute. A run tied to a fixed commit is read at that commit, which no fetch changes, except one
// that names the ref holding that commit, such as a pull request's plan: its commit may sit on no
// branch the checkout has fetched, so the ref is fetched, and the commit read where it landed.
//
// It fetches only when a rule in force reads what the run's playbook or configuration does, which
// a reversibility floor does for any run and a dry-run exclusion or a risk floor does for a dry
// run, so an install without one pays no fetch on the submit path. A failed fetch is logged and the
// gate grades what the checkout already holds: the execution that follows syncs the same remote and
// fails on the same fault, so it runs nothing the grade did not see.
func (d *Dispatcher) refreshForGate(policies []*policy.Policy, units ...*run.Run) {
	if d.projects == nil || d.syncer == nil {
		return
	}
	ctx := context.Background()
	fetched := map[string]bool{}
	for _, r := range units {
		if r == nil || r.ProjectID == "" || r.CommitSHA != "" || !scannable(r) ||
			!readsPlaybook(policies, r) {
			continue
		}
		if r.GitRef != "" {
			d.fetchRefForGate(ctx, r)
			continue
		}
		if fetched[r.ProjectID] || r.PinnedCommit != "" {
			continue
		}
		fetched[r.ProjectID] = true
		p, err := d.projects.Get(ctx, r.ProjectID)
		if err != nil {
			continue
		}
		sshKey, release, err := d.projectKey(ctx, p)
		if err == nil {
			_, err = d.syncer.Fetch(p, sshKey)
		}
		release()
		if err != nil {
			d.log.Warn("dispatch: fetch project for the gate: "+err.Error(), zap.String("project", p.ID))
		}
	}
}

// fetchRefForGate fetches the ref r names into its project's checkout, so the commit r is pinned
// to can be read before r runs. A failure is logged and left to the read, which then fails closed.
func (d *Dispatcher) fetchRefForGate(ctx context.Context, r *run.Run) {
	if d.projects == nil || d.syncer == nil || r.ProjectID == "" || r.GitRef == "" {
		return
	}
	p, err := d.projects.Get(ctx, r.ProjectID)
	if err != nil {
		d.log.Warn("dispatch: read project for the gate: "+err.Error(),
			zap.String("project", r.ProjectID))
		return
	}
	sshKey, release, err := d.projectKey(ctx, p)
	if err == nil {
		_, err = d.syncer.FetchRef(p, sshKey, r.GitRef)
	}
	release()
	if err != nil {
		d.log.Warn("dispatch: fetch ref for the gate: "+err.Error(), zap.String("project", p.ID))
	}
}

// readsPlaybook reports whether a rule in force decides on what r's playbook or configuration
// does. A reversibility floor does for every run, and so does a Rego policy that reads
// input.reversibility. For a dry run, so do a rule that excludes dry runs, a risk floor, and every
// Rego policy: the scan decides whether the dry run is change free, which decides whether the
// exclusion applies and how the run is graded, and a Rego policy judges a dry run that is not
// change free as the real run it may be as well as by what it reads. Nothing else in a rule reads a
// playbook's or a configuration's content.
func readsPlaybook(policies []*policy.Policy, r *run.Run) bool {
	for _, p := range policies {
		if p == nil {
			continue
		}
		if p.Reversibility != "" || (r.DryRun && (p.ExcludeDryRun || p.MinRisk != "")) {
			return true
		}
		if p.Rego != nil && (r.DryRun || p.Rego.Reads("reversibility")) {
			return true
		}
	}
	return false
}

// stampCommit records the commit a sync checked out and refuses a run pinned to a different one. The
// two are one function so a caller cannot record the commit without honoring the pin: the apply a plan
// gate proposes is pinned to the commit the approver's plan was read from, and a branch that moved in
// between must stop the run rather than apply code nobody judged.
func stampCommit(r *run.Run, sha string) error {
	r.CommitSHA = sha
	return checkPinnedCommit(r)
}

// checkPinnedCommit refuses a run whose project sync produced a commit other than the one the run is
// pinned to. An unpinned run, which is every ordinary one, passes; so does a pinned run that synced
// nothing, since there is no contradiction in that.
//
// The message names both commits because the operator's next step is to look at what changed between
// them and then re-run the plan.
func checkPinnedCommit(r *run.Run) error {
	if r.PinnedCommit == "" || r.CommitSHA == "" || r.PinnedCommit == r.CommitSHA {
		return nil
	}
	return fmt.Errorf("%w: this run was approved against commit %s and the project now holds %s, so "+
		"it would apply code that was never judged: run the plan again against the current commit",
		ErrCommitMoved, r.PinnedCommit, r.CommitSHA)
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
// The lease is handed back rather than released here. A dynamic engine mints a login that lives for
// the length of its lease, and these values are read by the container runner when it pulls, which is
// after this returns: releasing on the way out killed the credential before anything used it, so a
// registry login from Vault or its peers failed on every containerized run. Every other credential
// on the execute path already works this way, with one cleanup the caller defers beside the run.
//
// The returned cleanup is always safe to call, including on the error paths.
func (d *Dispatcher) resolvePullCredential(ctx context.Context, id string,
	spec *roundhouse.Spec) (cleanup func(), err error) {
	return d.resolvePullFrom(ctx, storeSource{d: d}, id, spec)
}

// resolvePullFrom is resolvePullCredential with the login taken from src, so a relay worker pulls
// with the login the control node delivered rather than one it has no store to open.
func (d *Dispatcher) resolvePullFrom(ctx context.Context, src secretSource, id string,
	spec *roundhouse.Spec) (cleanup func(), err error) {
	cleanup = func() {}
	if id == "" {
		return cleanup, nil
	}
	// Opened through the same path a run credential takes, so a credential whose source is an
	// external engine is resolved rather than used as its own config. Unsealing alone returned the
	// source configuration, and RegistryLogin read that JSON as a username, so an install keeping
	// registry logins in a secret store pulled with a garbage login and the run failed on an image
	// it was entitled to.
	_, plain, lease, err := src.credential(ctx, id)
	if err != nil {
		return cleanup, fmt.Errorf("pull credential %s: %w", id, err)
	}
	if lease != nil {
		cleanup = func() { d.revokeLease(lease) }
	}
	spec.RegistryUsername, spec.RegistryPassword = credential.RegistryLogin(plain)
	return cleanup, nil
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
