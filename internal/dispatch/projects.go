package dispatch

import (
	"context"
	"fmt"

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

// bindHeldRun binds a run an approval stands in front of to what that approval is shown. A run that
// names its playbook or its Terraform configuration by a path on the server is refused, a run drawn
// from a project is pinned to the commit the project holds now, and the run is graded again at that
// commit and held to gate, what the gate decided. Every submit path that can hold a run calls it
// once the hold is decided and before anything is saved, so a refused run is never stored, whoever
// or whatever asked for it.
func (d *Dispatcher) bindHeldRun(r *run.Run, gate gateVerdict) error {
	if err := refuseHeldPath(r); err != nil {
		return err
	}
	if err := d.pinHeldRunCommit(r); err != nil {
		return err
	}
	return d.regradeAtPin(r, gate)
}

// regradeAtPin grades a run an approval stands in front of again, at the commit it was just pinned
// to, and refuses it when the rules decide there anything other than gate, what the gate decided.
// Otherwise what the gate's scans read, what the rules noted, and the hold note are replaced with
// what the regrade wrote, so the evidence an approver reads describes the commit the approval
// binds.
//
// The gate reads the project's checkout and the pin fetches it again, so the two can see different
// commits: a push can land between the fetches, the gate's fetch can fail, or the project may never
// have been synced. Every one of those ends here in a read by hash of the pinned commit. A regrade
// that decides differently means the run was held, or not, on a commit it will not execute, so it
// is refused and the submitter tries again, which grades and pins one commit.
//
// The regrade runs on a copy carrying the status the gate saw, since the plan gate's rule reads the
// status and the run's own status has since moved to held. A run held by a ref fetches the ref
// first, so its commit is read rather than left unread.
func (d *Dispatcher) regradeAtPin(r *run.Run, gate gateVerdict) error {
	if !awaitsApproval(r) {
		return nil
	}
	ctx := context.Background()
	if r.GitRef != "" {
		d.fetchRefForGate(ctx, r)
	}
	policies, err := d.listPolicies(ctx)
	if err != nil {
		return fmt.Errorf("%w: approval policies could not be read, so the run is refused rather "+
			"than run past a gate that could not be checked: %w", ErrPolicyUnavailable, err)
	}
	c := r.Clone()
	c.Status, c.DryRunScans, c.PolicyNotes = gate.Status, nil, nil
	var v gateVerdict
	var rec gateRecord
	if r.Kind == run.KindPipeline {
		v, rec = d.decidePipeline(policies, c, c.Steps)
	} else {
		v, rec = d.decideHold(policies, c)
	}
	if v != gate || r.PolicySet == nil || v.PolicySet != r.PolicySet.Digest {
		return fmt.Errorf("%w: try again: the rules decide on this run differently at %s than "+
			"they did when it was submitted", ErrCommitUnpinned, pinnedAt(r))
	}
	r.DryRunScans, r.PolicyNotes, r.HoldNote = rec.DryRunScans, rec.PolicyNotes, rec.HoldNote
	return nil
}

// pinnedAt names the commit r is pinned to, for a refusal.
func pinnedAt(r *run.Run) string {
	if r.PinnedCommit == "" {
		return "this moment"
	}
	return "commit " + r.PinnedCommit[:min(12, len(r.PinnedCommit))]
}

// awaitsApproval reports whether a person's approval stands between r and some of what it runs: r
// is held, or r is a workflow with an approval step.
//
// A workflow's approval step counts here, for the pin and the path rule, and nowhere else. The
// workflow itself is not held: it starts at once, runs the steps ahead of the approval, and is
// graded by the rules like any other, so its status and policy pass do not change. Counting the
// step as a hold would make every workflow with an approval node wait for two approvals. Not
// counting it left the steps after the approval unpinned, so the person deciding the step released
// whatever the branch or the path held by the time it ran.
func awaitsApproval(r *run.Run) bool {
	return r.Status == run.StatusPendingApproval || run.HasApproval(r.Steps)
}

// refuseHeldPath refuses a run an approval stands in front of when it names its program by a path
// on the server rather than from a project.
//
// An approval binds the run's spec and, for a run drawn from a project, the commit it is pinned to.
// A playbook or a Terraform root named by path is read from disk when the run is claimed, so
// whatever the file holds then is what runs, and anyone who can write the file between the yes and
// the claim decides what the approval released. There is no exemption by source: a schedule, a
// trigger, and a drift reconcile are refused the same way a person and an agent are, and a schedule
// or trigger records the refusal as its last error.
//
// An Ansible playbook is covered, and so is a Terraform or OpenTofu configuration, which plans with
// provider and data source code when the run is released, unless the run carries a sealed plan: the
// plan file holds the configuration under the digest the approval binds. A script tool is not
// covered, since its program is the command text the spec seals and the approver reads whole.
func refuseHeldPath(r *run.Run) error {
	if !awaitsApproval(r) || r.ProjectID != "" {
		return nil
	}
	what := pathProgram(r.Tool, r.Playbook, r.Command, r.PlanSHA256)
	if r.Kind == run.KindPipeline {
		what = ""
		for _, s := range r.Steps {
			if s.IsApproval() {
				continue
			}
			if step := pathProgram(s.Tool, s.Playbook, s.Command, ""); step != "" {
				what = fmt.Sprintf("%s in workflow step %q", step, s.Name)
				break
			}
		}
	}
	if what == "" {
		return nil
	}
	return fmt.Errorf("%w: %s is named by a path on this server, and an approval cannot bind what "+
		"a path holds when the run is released: put it in a project and run it from there",
		ErrHeldByPath, what)
}

// pathProgram describes the program a run of tool reads from a path on the server when it executes,
// or returns empty when the program is sealed in the spec or carried by a sealed plan.
func pathProgram(tool, playbook, command, planSHA string) string {
	switch run.NormalizeTool(tool) {
	case run.ToolAnsible:
		return fmt.Sprintf("the playbook %q", playbook)
	case run.ToolTerraform, run.ToolOpenTofu:
		if planSHA != "" {
			return ""
		}
		return fmt.Sprintf("the configuration %q", command)
	}
	return ""
}

// pinHeldRunCommit stamps the commit a held git-backed run is allowed to execute, at the moment it
// is held, and refuses the run when that commit cannot be read.
//
// Approval binds the spec digest, but for a run drawn from a git project the spec names a branch,
// and the branch is a moving pointer: between the approver's yes and the worker's claim, HEAD can
// advance, so the release executed code nobody judged. The plan gate already pins its proposals;
// blanket holds did not. Pinning at hold time makes the approver's decision mean the commit that
// was current when the request was made, and execution already refuses a pin that no longer
// matches.
//
// A run whose commit cannot be pinned is refused rather than held. Held unpinned, its approval
// would release whatever the branch holds at execution time, which is code nobody was shown. The
// caller saves nothing and the submitter is told to try again, which succeeds once the project's
// repository can be fetched.
//
// The pin only moves the project's checkout and reads the commit. It installs none of the
// project's dependencies, since the run is held and nothing is installed for it before a person
// approves: the sync at claim installs them for the commit pinned here.
func (d *Dispatcher) pinHeldRunCommit(r *run.Run) error {
	if !awaitsApproval(r) || r.ProjectID == "" || r.PinnedCommit != "" || r.GitRef != "" {
		return nil
	}
	if d.projects == nil || d.syncer == nil {
		return fmt.Errorf("%w: projects are not enabled on this server: %w", ErrCommitUnpinned,
			project.ErrNotFound)
	}
	fail := func(err error) error {
		return fmt.Errorf("%w: try again: project %s: %w", ErrCommitUnpinned, r.ProjectID, err)
	}
	p, err := d.projects.Get(context.Background(), r.ProjectID)
	if err != nil {
		return fail(err)
	}
	// Resolved and unlocked the same way the sync during execution does. Pinning the commit runs the
	// identical git operation, so a credential that works for one and not the other would refuse a
	// run at hold time for a reason that disappears by execution.
	sshKey, release, err := d.projectKey(context.Background(), p)
	defer release()
	if err != nil {
		return fail(err)
	}
	sha, err := d.syncer.Fetch(p, sshKey)
	if err != nil {
		return fail(err)
	}
	r.PinnedCommit = sha
	return nil
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
// run, so an install without one pays no fetch on the submit path. The fetch installs none of the
// project's dependencies, which only the sync for a claimed run does.
//
// A failed fetch of a branch refuses the submission, held or not, and the submitter tries again.
// The checkout would otherwise be graded at whatever its last sync left, of any age, and a run that
// is not held is claimed later by a sync of its own, often in another process, which can succeed
// where this fetch failed and run a commit no rule graded. A gate that cannot be evaluated has not
// passed. A run tied to a ref is not refused here: it is read by hash, and a commit the fetch did
// not bring in reads as unread, which the rules treat as the worst case.
func (d *Dispatcher) refreshForGate(policies []*policy.Policy, units ...*run.Run) error {
	if d.projects == nil || d.syncer == nil {
		return nil
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
		if err := d.fetchForGate(ctx, r.ProjectID); err != nil {
			d.log.Warn("dispatch: fetch project for the gate: "+err.Error(),
				zap.String("project", r.ProjectID))
			return fmt.Errorf("%w: try again: project %s could not be fetched to read what the run "+
				"executes: %w", ErrCommitUnpinned, r.ProjectID, err)
		}
	}
	return nil
}

// fetchForGate fetches the branch of the project with id into its checkout.
func (d *Dispatcher) fetchForGate(ctx context.Context, id string) error {
	p, err := d.projects.Get(ctx, id)
	if err != nil {
		return err
	}
	sshKey, release, err := d.projectKey(ctx, p)
	defer release()
	if err != nil {
		return err
	}
	_, err = d.syncer.Fetch(p, sshKey)
	return err
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
	return d.resolvePullFrom(ctx, storeSource{d: d}, nil, id, spec)
}

// resolvePullFrom is resolvePullCredential with the login taken from src, so a relay worker pulls
// with the login the control node delivered rather than one it has no store to open. r is the
// claimed run the login is opened to execute, whose claim a minted login's recorded handle is bound
// to, or nil for a login opened outside any claim, which is recorded nowhere.
func (d *Dispatcher) resolvePullFrom(ctx context.Context, src secretSource, r *run.Run, id string,
	spec *roundhouse.Spec) (cleanup func(), err error) {
	cleanup = func() {}
	if id == "" {
		return cleanup, nil
	}
	// The record is read and checked before anything is opened. A credential that is not a registry
	// login fails the run, and a login bound to another registry than the one serving the image is
	// left closed and off the spec, so the pull goes ahead without it.
	c, err := src.record(ctx, id)
	if err != nil {
		return cleanup, fmt.Errorf("pull credential %s: %w", id, err)
	}
	if err := credential.CheckPull(c); err != nil {
		return cleanup, err
	}
	if pullHost(c, spec.Image) == "" {
		d.log.Warn("dispatch: pull login left off the run: it is bound to another registry than "+
			"the image's", zap.String("credential", id),
			zap.String("registry_host", c.RegistryHost()), zap.String("image", spec.Image))
		return cleanup, nil
	}
	// Opened through the same path a run credential takes, so a credential whose source is an
	// external engine is resolved rather than used as its own config. Unsealing alone returned the
	// source configuration, and RegistryLogin read that JSON as a username, so an install keeping
	// registry logins in a secret store pulled with a garbage login and the run failed on an image
	// it was entitled to.
	opened, plain, lease, err := src.credential(ctx, id)
	if err != nil {
		return cleanup, fmt.Errorf("pull credential %s: %w", id, err)
	}
	if lease != nil {
		leases := &runLeases{d: d, r: r}
		cleanup = leases.release
		if lerr := leases.add(ctx, id, lease); lerr != nil {
			return cleanup, fmt.Errorf("pull credential %s: %w", id, lerr)
		}
	}
	// The record the secret was opened with decides again, so a credential edited between the check
	// and the open still reaches only the registry its own record names.
	if pullHost(opened, spec.Image) == "" {
		return cleanup, nil
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
