package dispatch

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// SecretDelivery hands an executor that holds no credential store the secrets the control node
// opened and sealed for one run when it was claimed. A relay worker uses it: its transport keeps
// what each claim delivered, and this opens it, in memory, at the moment the run first needs a
// secret.
type SecretDelivery interface {
	// Receive opens the secrets delivered with r's claim. It returns a nil payload and a nil error
	// when the claim carried none, which is what a run needing no secret gets, and an error when the
	// control node refused to send them or what arrived does not open for this claim. A delivery is
	// opened at most once.
	Receive(ctx context.Context, r *run.Run) (*handoff.Payload, error)
	// Discard wipes whatever is still held for the run, opened or not. It is safe to call more than
	// once and for a run that was delivered nothing.
	Discard(runID string)
}

// WithSecretDelivery makes the dispatcher take each run's secrets from what the control node sealed
// at claim, instead of opening them from a credential store it does not have. A relay worker always
// sets it, so a run whose secrets were refused or cannot be opened fails with the reason.
func WithSecretDelivery(sd SecretDelivery) Option {
	return func(c *config) { c.delivery = sd }
}

// secretSource supplies the opened secrets one execution needs. An executor with a credential store
// opens them itself. A relay worker takes them from what the control node sealed at claim. Every
// credential is applied through this one seam, so a secret delivered to a relay worker lands in the
// same run directory, in the same form, masked the same way, as one a database-backed worker opens.
type secretSource interface {
	// credentialIDs returns the credentials the execution materializes, in order, or why this
	// executor cannot materialize them.
	credentialIDs(ctx context.Context, r *run.Run) ([]string, error)
	// credential returns the credential with id and its resolved value, with a lease when the value
	// was minted for this run. A federated credential comes back with errFederatedCredential.
	credential(ctx context.Context, id string) (*credential.Credential, string, *secretsource.Lease,
		error)
	// credentialType returns the custom type the typed credential c names.
	credentialType(ctx context.Context, c *credential.Credential) (*credential.CredentialType, error)
	// federated delivers the run identity token for the federated credential c into dir.
	federated(ctx context.Context, r *run.Run, c *credential.Credential, dryRun bool, dir string,
		seen map[credential.Kind]string) (federation.Delivery, error)
	// answers returns the run's opened secret survey answers, keyed by variable name.
	answers(ctx context.Context, r *run.Run) (map[string]string, error)
	// inventorySnapshot returns the inventory content r was submitted with, held to the record r
	// binds, or why it cannot be had.
	inventorySnapshot(ctx context.Context, r *run.Run) (string, error)
	// planFile returns the plan file the gated apply r carries out, held to the digest r binds, or
	// why it cannot be had.
	planFile(ctx context.Context, r *run.Run) ([]byte, error)
	// wipe drops every opened value the source still holds.
	wipe()
}

// secretsFor returns where r's secrets come from on this executor.
func (d *Dispatcher) secretsFor(r *run.Run) secretSource {
	if d.delivery != nil {
		return &deliveredSource{r: r, recv: d.delivery}
	}
	return storeSource{d: d}
}

// discardDelivery wipes what the control node delivered for a run, on an executor that takes
// deliveries. It runs when the run's execution ends, however it ends, including the ends that never
// reach a secret: a lost start fence, a refused spec binding, a cancel before start.
func (d *Dispatcher) discardDelivery(runID string) {
	if d.delivery != nil {
		d.delivery.Discard(runID)
	}
}

// storeSource opens secrets from this executor's own credential store with its own key, which is
// how the control node and every worker with database access run.
type storeSource struct {
	// d is the dispatcher whose stores and sealer open the secrets.
	d *Dispatcher
}

// credentialIDs returns the run's credentials and its inventory's, refusing up front when there are
// some and this executor holds no key to open them.
func (s storeSource) credentialIDs(ctx context.Context, r *run.Run) ([]string, error) {
	ids := s.d.effectiveCredentialIDs(ctx, r)
	if len(ids) > 0 && (s.d.credentials == nil || s.d.sealer == nil) {
		return nil, credential.ErrNoKey
	}
	return ids, nil
}

// credential opens one credential from the store.
func (s storeSource) credential(ctx context.Context, id string) (*credential.Credential, string,
	*secretsource.Lease, error) {
	return s.d.openCredential(ctx, id)
}

// credentialType reads the type a typed credential names from the type store.
func (s storeSource) credentialType(ctx context.Context, c *credential.Credential) (*credential.CredentialType, error) {
	if s.d.credentialTypes == nil {
		return nil, fmt.Errorf("materialize credential %s: it names a custom type but none are "+
			"configured", c.ID)
	}
	typ, err := s.d.credentialTypes.Get(ctx, c.TypeID)
	if err != nil {
		return nil, fmt.Errorf("materialize credential %s: read type %s: %w", c.ID, c.TypeID, err)
	}
	return typ, nil
}

// federated mints the run's identity token on this process and delivers it.
func (s storeSource) federated(ctx context.Context, r *run.Run, c *credential.Credential, dryRun bool,
	dir string, seen map[credential.Kind]string) (federation.Delivery, error) {
	return s.d.deliverFederated(ctx, r, c, dryRun, dir, seen)
}

// answers opens the run's sealed survey answers with this executor's key.
func (s storeSource) answers(_ context.Context, r *run.Run) (map[string]string, error) {
	return s.d.openSecretVars(r)
}

// inventorySnapshot opens the run's sealed inventory snapshot with this executor's key.
func (s storeSource) inventorySnapshot(_ context.Context, r *run.Run) (string, error) {
	return s.d.openSnapshot(r)
}

// planFile opens the gated apply's sealed plan file with this executor's key.
func (s storeSource) planFile(_ context.Context, r *run.Run) ([]byte, error) {
	return s.d.openPlanFile(r)
}

// wipe has nothing to drop: a store-backed executor opens each value at the moment it is applied.
func (storeSource) wipe() {}

// deliveredSource takes a run's secrets from what the control node sealed to this worker's pool at
// claim. It opens the delivery once, at the first secret the run needs, so a run that ends before
// needing one never decrypts anything.
type deliveredSource struct {
	// r is the run the secrets belong to.
	r *run.Run
	// recv opens the run's delivery.
	recv SecretDelivery
	// loaded reports whether the delivery has been asked for, so it is opened at most once.
	loaded bool
	// wiped reports whether the opened values have been dropped.
	wiped bool
	// payload is the opened delivery, nil when the claim carried none.
	payload *handoff.Payload
	// err is why the delivery could not be opened.
	err error
}

// load opens the run's delivery the first time any secret is needed and returns it after that.
func (s *deliveredSource) load(ctx context.Context) (*handoff.Payload, error) {
	if s.wiped {
		return nil, fmt.Errorf("%w: this run's delivered secrets were already wiped", ErrNotDelivered)
	}
	if !s.loaded {
		s.loaded = true
		s.payload, s.err = s.recv.Receive(ctx, s.r)
	}
	return s.payload, s.err
}

// credentialIDs returns the credentials the control node opened for the run, in the order it would
// materialize them, after checking that every credential the run itself names is among them.
func (s *deliveredSource) credentialIDs(ctx context.Context, r *run.Run) ([]string, error) {
	p, err := s.load(ctx)
	switch {
	case err != nil:
		return nil, err
	case p == nil && len(r.CredentialIDs) > 0:
		return nil, fmt.Errorf("%w: this run names credentials and the control node sent none with "+
			"its claim, so they cannot be applied on this worker", ErrNotDelivered)
	case p == nil:
		return nil, nil
	}
	for _, id := range r.CredentialIDs {
		if p.Credential(id) == nil {
			return nil, fmt.Errorf("%w: credential %s, which this run names, was not delivered",
				ErrNotDelivered, id)
		}
	}
	return p.CredentialIDs, nil
}

// credential returns a delivered credential and its value. Its source was already resolved on the
// control node, so nothing is resolved here and no lease is held on this worker.
func (s *deliveredSource) credential(ctx context.Context, id string) (*credential.Credential, string,
	*secretsource.Lease, error) {
	p, err := s.load(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	dc := p.Credential(id)
	if dc == nil {
		return nil, "", nil, fmt.Errorf("%w: credential %s was not delivered with this run's claim",
			ErrNotDelivered, id)
	}
	c := dc.Record
	if credential.Federated(c.Kind) {
		return &c, "", nil, fmt.Errorf("%w: credential %q is %s, which mints a token for a run's "+
			"environment and cannot stand in for a stored secret here", errFederatedCredential, c.Name,
			c.Kind)
	}
	return &c, dc.Value, nil, nil
}

// credentialType returns the custom type the control node delivered beside a typed credential.
func (s *deliveredSource) credentialType(ctx context.Context,
	c *credential.Credential) (*credential.CredentialType, error) {
	p, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	typ := p.Type(c.TypeID)
	if typ == nil {
		return nil, fmt.Errorf("%w: materialize credential %s: its type %s was not delivered",
			ErrNotDelivered, c.ID, c.TypeID)
	}
	return typ, nil
}

// federated delivers the identity token the control node minted for a federated credential. The
// token was minted for one execution mode, a plan or an apply, and a worker about to execute in the
// other refuses it: a token whose claims say plan must never authorize an apply, and a cloud trust
// policy written against those claims relies on that.
func (s *deliveredSource) federated(ctx context.Context, r *run.Run, c *credential.Credential,
	dryRun bool, dir string, seen map[credential.Kind]string) (federation.Delivery, error) {
	p, err := s.load(ctx)
	if err != nil {
		return federation.Delivery{}, err
	}
	dc := p.Credential(c.ID)
	if dc == nil || dc.Token == "" {
		return federation.Delivery{}, fmt.Errorf("%w: no identity token was delivered for credential "+
			"%q", ErrNotDelivered, c.Name)
	}
	if p.DryRun != dryRun {
		return federation.Delivery{}, fmt.Errorf("%w: the identity token for credential %q was minted "+
			"for %s, and this execution is %s, so it is refused", ErrNotDelivered, c.Name,
			executionMode(p.DryRun), executionMode(dryRun))
	}
	if prior, dup := seen[c.Kind]; dup && prior != c.ID {
		return federation.Delivery{}, fmt.Errorf("%w: %s and %s are both %s", federation.ErrDuplicate,
			prior, c.ID, c.Kind)
	}
	seen[c.Kind] = c.ID
	cfg, err := federation.ParseSettings(c.Kind, c.Settings)
	if err != nil {
		return federation.Delivery{}, fmt.Errorf("credential %q: %w", c.Name, err)
	}
	// The control node minted the token for this run executing, so the session it opens is named
	// after the run, the way a token minted where it runs names it.
	delivery, err := federation.DeliverToken(ctx, cfg,
		federation.Claims{RunID: r.ID, Purpose: federation.PurposeRun}, dc.Token, dir)
	if err != nil {
		return delivery, fmt.Errorf("credential %q: %w", c.Name, err)
	}
	return delivery, nil
}

// executionMode names a dry-run flag the way a refusal reads it.
func executionMode(dryRun bool) string {
	if dryRun {
		return "a plan"
	}
	return "an apply"
}

// answers returns the delivered survey answers for every secret variable the run names. A name
// without an answer fails the run: a tool handed an empty password does something nobody chose.
func (s *deliveredSource) answers(ctx context.Context, r *run.Run) (map[string]string, error) {
	if len(r.SealedNames) == 0 {
		return nil, nil
	}
	p, err := s.load(ctx)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrSecretAnswer, err)
	case p == nil:
		return nil, fmt.Errorf("%w: this run carries secret survey answers and the control node "+
			"sent none with its claim", ErrSecretAnswer)
	}
	out := make(map[string]string, len(r.SealedNames))
	for _, name := range r.SealedNames {
		v, ok := p.Answers[name]
		if !ok {
			return nil, fmt.Errorf("%w: the answer to %q was not delivered", ErrSecretAnswer, name)
		}
		out[name] = v
	}
	return out, nil
}

// inventorySnapshot returns the inventory snapshot the control node opened for the run and sealed
// to this worker's pool, after holding it to the masked digest the run binds. The control node held
// the sealed form to the run's other digest before it opened it.
func (s *deliveredSource) inventorySnapshot(ctx context.Context, r *run.Run) (string, error) {
	p, err := s.load(ctx)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrInventorySnapshot, err)
	case p == nil || p.Inventory == "":
		return "", fmt.Errorf("%w: this run's inventory snapshot was not delivered with its claim",
			ErrInventorySnapshot)
	}
	if err := checkSnapshotContent(r, p.Inventory); err != nil {
		return "", err
	}
	return p.Inventory, nil
}

// planFile returns the plan file the control node opened for the gated apply and sealed to this
// worker's pool. The control node held its sealed form to the digest the apply binds before it
// opened it. The caller gets its own copy: the delivery is wiped in place once the run's
// credentials are applied, and a slice sharing its bytes would be zeroed under the caller.
func (s *deliveredSource) planFile(ctx context.Context, _ *run.Run) ([]byte, error) {
	p, err := s.load(ctx)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrPlanFile, err)
	case p == nil || len(p.PlanFile) == 0:
		return nil, fmt.Errorf("%w: this apply's plan file was not delivered with its claim",
			ErrPlanFile)
	}
	return slices.Clone(p.PlanFile), nil
}

// wipe drops the opened delivery. Anything asked of the source afterward is refused rather than
// opened a second time.
func (s *deliveredSource) wipe() {
	s.payload.Wipe()
	s.payload, s.wiped = nil, true
}

// NeedsSecrets reports whether r carries a secret its executor would open: a credential of its own
// or of the inventory it targets, the registry login its image is pulled with, or a secret survey
// answer. The relay asks it at claim to decide whether there is anything to deliver at all.
func (d *Dispatcher) NeedsSecrets(ctx context.Context, r *run.Run) bool {
	return len(d.effectiveCredentialIDs(ctx, r)) > 0 || len(r.SealedNames) > 0 ||
		len(r.SealedVars) > 0 || (r.Image != "" && r.PullCredentialID != "") ||
		r.InventorySnapshot != nil || r.PlanSHA256 != ""
}

// OpenSecrets opens every secret r needs, exactly the set its executor would open, for the control
// node to seal to the relay worker pool that claimed it. It returns the opened payload and a
// release that hands back anything the opening minted, such as a dynamic secret's lease, which the
// caller runs when the run ends. The release is nil when nothing was minted.
//
// Credential sources are resolved here, on the control node, as an executor with database access
// resolves them, so a worker in an isolated segment needs no route to Vault or a cloud secret
// manager. A federated credential's identity token is minted here too, with the signing key that
// never leaves this side, for the execution mode the worker will run: a plan when the run is a dry
// run or an apply the plan gate plans first.
func (d *Dispatcher) OpenSecrets(ctx context.Context, r *run.Run) (*handoff.Payload, func(), error) {
	p := &handoff.Payload{RunID: r.ID}
	leases := &runLeases{d: d, r: r}
	fail := func(err error) (*handoff.Payload, func(), error) {
		p.Wipe()
		leases.release()
		return nil, nil, err
	}
	ids := d.effectiveCredentialIDs(ctx, r)
	pull := ""
	if r.Image != "" {
		pull = r.PullCredentialID
	}
	noKey := d.credentials == nil || d.sealer == nil || !d.sealer.Enabled()
	if (len(ids) > 0 || pull != "") && noKey {
		return fail(credential.ErrNoKey)
	}
	p.CredentialIDs = ids
	seen := map[credential.Kind]string{}
	modeKnown := false
	for _, id := range ids {
		if p.Credential(id) != nil {
			continue
		}
		c, plain, lease, err := d.openCredential(ctx, id)
		if lease != nil {
			if lerr := leases.add(ctx, id, lease); lerr != nil {
				return fail(lerr)
			}
		}
		switch {
		case errors.Is(err, errFederatedCredential):
			if !modeKnown {
				dryRun, merr := d.deliveryMode(ctx, r)
				if merr != nil {
					return fail(merr)
				}
				p.DryRun, modeKnown = dryRun, true
			}
			cfg, ferr := d.checkFederated(c, seen)
			if ferr != nil {
				return fail(ferr)
			}
			token, merr := d.federation.MintFor(ctx, cfg, d.runClaims(ctx, r, c.ID, p.DryRun))
			if merr != nil {
				return fail(fmt.Errorf("credential %q: %w", c.Name, merr))
			}
			dc := handoff.NewCredential(c, "")
			dc.Token = token
			p.Credentials = append(p.Credentials, dc)
		case err != nil:
			return fail(err)
		default:
			p.Credentials = append(p.Credentials, handoff.NewCredential(c, plain))
			if c.TypeID != "" {
				typ, terr := storeSource{d: d}.credentialType(ctx, c)
				if terr != nil {
					return fail(terr)
				}
				p.AddType(typ)
			}
		}
	}
	if pull != "" && p.Credential(pull) == nil {
		c, plain, lease, err := d.openCredential(ctx, pull)
		if lease != nil {
			if lerr := leases.add(ctx, pull, lease); lerr != nil {
				return fail(lerr)
			}
		}
		if err != nil {
			return fail(fmt.Errorf("pull credential %s: %w", pull, err))
		}
		p.Credentials = append(p.Credentials, handoff.NewCredential(c, plain))
	}
	answers, err := d.openSecretVars(r)
	if err != nil {
		return fail(err)
	}
	p.Answers = answers
	// The inventory snapshot and the plan file are opened here, each held to the digest the run binds
	// first, so a worker is handed exactly what the run was submitted and approved with.
	if r.InventorySnapshot != nil {
		content, serr := d.openSnapshot(r)
		if serr != nil {
			return fail(serr)
		}
		p.Inventory = content
	}
	if r.PlanSHA256 != "" {
		plan, perr := d.openPlanFile(r)
		if perr != nil {
			return fail(perr)
		}
		p.PlanFile = plan
	}
	if leases.empty() {
		return p, nil, nil
	}
	return p, leases.release, nil
}

// deliveryMode reports whether r will execute as a plan on the worker that claims it: a dry run, or
// an apply a rule plans first. The worker reads the same rules across the relay and refuses a token
// minted for the other mode, so a rule edited in between fails the run closed rather than handing a
// plan's token to an apply.
func (d *Dispatcher) deliveryMode(ctx context.Context, r *run.Run) (bool, error) {
	if r.DryRun {
		return true, nil
	}
	policies, err := d.planGatePolicies(ctx, r)
	if err != nil {
		return false, err
	}
	return policies != nil, nil
}
