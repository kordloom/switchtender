package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// A composed inventory is resolved by the engine its definition calls for, never by what happens to
// be installed. A smart inventory's filter is evaluated here, over inputs the native engine reads
// whenever an input is a self-contained static document, and over Ansible's reading of an input
// only when the input itself needs Ansible, such as a plugin configuration or a vault-encrypted
// value. A constructed inventory is always Ansible's: its options are Jinja, which is run by
// Ansible's constructed plugin and never reimplemented. When Ansible is needed and absent, the
// error says why it is needed and how to install it.

// composeWorkers bounds how many inputs Ansible lists at once. Each listing is one
// ansible-inventory process, so the bound keeps a smart inventory over many Ansible-read inputs
// from starting a process per inventory at the same moment.
const composeWorkers = 4

// composeTimeout bounds one resolution, so an input whose content source hangs cannot hold a launch
// or a preview open forever.
const composeTimeout = 2 * time.Minute

// Composition is what a composed inventory resolved to: the record a run keeps, and the inventory
// document handed to the executor.
type Composition struct {
	// Resolution is the kind, the inputs drawn from, and the hosts resolved to.
	Resolution *run.InventoryResolution `json:"resolution"`
	// Content is the resolved inventory, as JSON Ansible's YAML plugin reads. It holds the hosts'
	// variables, so it is handed to the executor and never served.
	Content string `json:"-"`
}

// composeInput is one input inventory read for a composition.
type composeInput struct {
	// inv is the stored input inventory.
	inv *inventory.Inventory
	// path is where its content was written for ansible-inventory.
	path string
	// content is its content as resolved for this composition.
	content string
	// listing is its resolution, by whichever engine read it.
	listing *inventory.Listing
	// engine is the engine that read it: native or ansible.
	engine string
}

// composed is a composition with what the executor's cross-check needs beside it: the composed
// listing as rendered, and every input with the engine that read it.
type composed struct {
	// Composition is the record and the rendered content.
	*Composition
	// listing is the composed inventory as rendered into Content.
	listing *inventory.Listing
	// inputs are the inputs read, in order.
	inputs []*composeInput
}

// PreviewInventory resolves a composed inventory definition, saved or not, for the actor on ctx,
// exactly as a launch by that actor would. It is how a person sees which hosts a filter selects
// before saving it, and it draws only on inputs that person may use.
func (d *Dispatcher) PreviewInventory(ctx context.Context, inv *inventory.Inventory) (*Composition, error) {
	if err := inventory.Validate(inv); err != nil {
		return nil, err
	}
	if !inv.Composed() {
		return nil, fmt.Errorf("%w: only a smart or constructed inventory is resolved",
			inventory.ErrComposition)
	}
	c, err := d.compose(ctx, inv, run.InventoryAccessFrom(ctx), nil)
	if err != nil {
		return nil, err
	}
	return c.Composition, nil
}

// resolveComposed records on r the hosts its composed inventory resolves to for the actor on ctx,
// clearing any resolution carried in from an earlier launch, since every launch resolves afresh,
// and snapshots what it resolved to: the composed result and every input it drew from, each read
// once, now. A run against a static inventory, or against none, is left alone. A run derived from
// another, the apply a plan proposes, arrives carrying its source's resolution and snapshot and
// keeps both, since it is part of the same submission.
//
// It runs before the policy pass, so a rule scoped to an input inventory sees the run as touching
// that inventory's hosts, and before the run is stored, so the record the approver reads and the
// inventory execution runs are the same.
func (d *Dispatcher) resolveComposed(ctx context.Context, r *run.Run) error {
	if r.InventoryResolution != nil && r.InventorySnapshot != nil && r.InventorySealed != "" {
		return nil
	}
	r.InventoryResolution = nil
	if r.InventoryID == "" || d.inventories == nil {
		return nil
	}
	inv, err := d.inventories.Get(ctx, r.InventoryID)
	if err != nil {
		return fmt.Errorf("%w: %s", err, r.InventoryID)
	}
	if !inv.Composed() {
		return nil
	}
	access := run.InventoryAccessFrom(ctx)
	c, err := d.compose(ctx, inv, access, nil)
	if err != nil {
		return err
	}
	if len(c.Resolution.Hosts) == 0 {
		// A schedule or a webhook has no person behind it and reads every input in scope, so its
		// message names no actor; a person's launch matched nothing that person may use.
		if access == nil {
			return fmt.Errorf("%w: %s matched no hosts", inventory.ErrNoHosts, inv.Name)
		}
		return fmt.Errorf("%w: %s matched nothing the launching actor may use", inventory.ErrNoHosts,
			inv.Name)
	}
	r.InventoryResolution = c.Resolution
	return d.snapshotComposed(r, inv, c)
}

// composedContent renders a composed inventory held to the resolution a run recorded when it
// launched: only the inputs it drew from are read and only the hosts it resolved to are kept. It is
// how the retry of a split's failed shards takes its snapshot. A composed run with no resolution is
// refused rather than resolved here, because no actor is asked, and resolving without one would
// reach every host on the install.
func (d *Dispatcher) composedContent(ctx context.Context, inv *inventory.Inventory,
	pinned *run.InventoryResolution) (*composed, error) {
	if pinned == nil {
		return nil, fmt.Errorf("%w: %s was not resolved when the run launched", inventory.ErrResolve,
			inv.Name)
	}
	return d.compose(ctx, inv, nil, pinned)
}

// compose resolves inv. access, when set, drops every input the actor may not use. pinned, when
// set, holds the result to a resolution recorded earlier.
func (d *Dispatcher) compose(ctx context.Context, inv *inventory.Inventory, access run.InventoryAccessFunc,
	pinned *run.InventoryResolution) (*composed, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	ctx = d.bindAnsible(ctx)
	inputs, err := d.composeInputs(ctx, inv, access, pinned)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "switchtender-compose-*")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var secrets []string
	for n, in := range inputs {
		content, err := d.inventoryContent(ctx, in.inv)
		if err != nil {
			return nil, fmt.Errorf("%w: input %s: %w", inventory.ErrResolve, in.inv.Name, err)
		}
		secrets = append(secrets, inventory.Secrets(content)...)
		in.content = content
		in.path = filepath.Join(dir, fmt.Sprintf("input-%03d", n))
		if err := os.WriteFile(in.path, []byte(content), 0o600); err != nil {
			return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
		}
	}
	var (
		listing     *inventory.Listing
		drawn       []string
		ansibleCore string
	)
	switch inv.Kind {
	case inventory.KindSmart:
		listing, drawn, ansibleCore, err = d.composeSmart(ctx, inv, inputs)
	case inventory.KindConstructed:
		listing, drawn, ansibleCore, err = d.composeConstructed(ctx, inv, inputs, dir)
	default:
		err = fmt.Errorf("%w: %s is not a composed inventory", inventory.ErrComposition, inv.Name)
	}
	if err != nil {
		return nil, maskComposeError(err, secrets)
	}
	if pinned != nil {
		keep := make(map[string]bool, len(pinned.Hosts))
		for _, h := range pinned.Hosts {
			keep[h] = true
		}
		listing = listing.Restrict(keep)
	}
	content, err := listing.Static()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	resolved, err := listing.Digest()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	res := &run.InventoryResolution{
		Kind: inv.Kind, Inputs: drawn, Hosts: listing.Hosts(), Engine: inventory.EngineNative,
		ResolvedDigest: resolved,
	}
	if ansibleCore != "" {
		res.Engine, res.AnsibleCore = inventory.EngineAnsible, ansibleCore
		res.AnsibleSource = d.ansibleSourceFor(ctx)
	}
	if pinned != nil {
		res.Inputs = slices.Clone(pinned.Inputs)
	}
	res.InputDigest = inputDigest(inputs, res.Inputs)
	return &composed{
		Composition: &Composition{Resolution: res, Content: string(content)},
		listing:     listing, inputs: inputs,
	}, nil
}

// inputDigest returns the digest of the inputs named in drawn, in that order, from what was read.
func inputDigest(inputs []*composeInput, drawn []string) string {
	byID := make(map[string]*composeInput, len(inputs))
	for _, in := range inputs {
		byID[in.inv.ID] = in
	}
	list := make([]inventory.DigestInput, 0, len(drawn))
	for _, id := range drawn {
		if in := byID[id]; in != nil {
			list = append(list, inventory.DigestInput{ID: id, Content: in.content})
		}
	}
	return inventory.InputDigest(list)
}

// composeInputs returns the inventories a composition reads, in order.
//
// A pinned run reads the inputs its launch drew from and nothing else. Otherwise a smart inventory
// reads every static inventory, in its own organization when it has one, and a constructed one
// reads its named inputs. Either way an input the actor may not use is dropped, so composing can
// never reach a host the actor could not have targeted by naming its inventory directly.
func (d *Dispatcher) composeInputs(ctx context.Context, inv *inventory.Inventory, access run.InventoryAccessFunc,
	pinned *run.InventoryResolution) ([]*composeInput, error) {
	var candidates []*inventory.Inventory
	switch {
	case pinned != nil:
		for _, id := range pinned.Inputs {
			in, err := d.inventories.Get(ctx, id)
			if errors.Is(err, inventory.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%w: input %s: %w", inventory.ErrResolve, id, err)
			}
			candidates = append(candidates, in)
		}
	case inv.Kind == inventory.KindSmart:
		all, err := d.inventories.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: list inventories: %w", inventory.ErrResolve, err)
		}
		for _, in := range all {
			if in.ID == inv.ID || in.Composed() || (inv.OrgID != "" && in.OrgID != inv.OrgID) {
				continue
			}
			candidates = append(candidates, in)
		}
	default:
		for _, id := range inv.InputIDs {
			in, err := d.inventories.Get(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("%w: input %s: %w", inventory.ErrComposition, id, err)
			}
			if in.Composed() {
				return nil, fmt.Errorf("%w: input %s is itself a %s inventory", inventory.ErrComposition,
					in.Name, in.Kind)
			}
			candidates = append(candidates, in)
		}
	}
	out := make([]*composeInput, 0, len(candidates))
	for _, in := range candidates {
		if pinned == nil && access != nil {
			ok, err := access(ctx, in.ID)
			if err != nil {
				return nil, fmt.Errorf("%w: check access to %s: %w", inventory.ErrResolve, in.ID, err)
			}
			if !ok {
				continue
			}
		}
		out = append(out, &composeInput{inv: in})
	}
	return out, nil
}

// needsAnsibleError says that resolving what is named needs Ansible, why, and how to install it.
func (d *Dispatcher) needsAnsibleError(what, why string) error {
	return fmt.Errorf("%w: %w: %s needs Ansible because %s, and ansible-inventory is not installed "+
		"on this server. Install it with: %s", inventory.ErrResolve, inventory.ErrNeedsAnsible, what,
		why, d.installHint())
}

// AnsibleCommands reports where this process's Ansible commands come from, a configured directory,
// the managed runtime, or PATH, and false when its runner cannot say.
func (d *Dispatcher) AnsibleCommands() (ansibleruntime.Commands, bool) {
	r, ok := d.runner.(roundhouse.AnsibleCommandsReporter)
	if !ok {
		return ansibleruntime.Commands{}, false
	}
	return r.AnsibleCommands(), true
}

// installHint returns the line that installs Ansible for this process, naming the managed
// runtime's directory it looks in.
func (d *Dispatcher) installHint() string {
	c, _ := d.AnsibleCommands()
	return inventory.AnsibleInstallHintFor(c.Root)
}

// bindAnsible binds the host's Ansible, located once, to ctx, so every Ansible command started for
// the work ctx carries, and the source its evidence records, name the same one whatever an install
// or remove does meanwhile. A context with one bound already keeps it.
func (d *Dispatcher) bindAnsible(ctx context.Context) context.Context {
	if _, ok := roundhouse.AnsibleCommandsFrom(ctx); ok {
		return ctx
	}
	cmds, ok := d.AnsibleCommands()
	if !ok {
		return ctx
	}
	return roundhouse.WithAnsibleCommands(ctx, cmds)
}

// ansibleSourceFor returns where the Ansible bound to ctx comes from, as a run's evidence names it,
// and empty when none is bound.
func (d *Dispatcher) ansibleSourceFor(ctx context.Context) string {
	if cmds, ok := roundhouse.AnsibleCommandsFrom(ctx); ok {
		return cmds.Source
	}
	return ""
}

// ansibleCore returns the server's ansible-core version, or a needsAnsibleError naming what needs
// it when Ansible is not installed.
func (d *Dispatcher) ansibleCore(ctx context.Context, what, why string) (string, error) {
	if d.invLister == nil || d.ansibleCoreReporter == nil {
		return "", d.needsAnsibleError(what, why)
	}
	v, err := d.ansibleCoreReporter.AnsibleCoreVersion(ctx)
	if errors.Is(err, roundhouse.ErrAnsibleMissing) {
		return "", d.needsAnsibleError(what, why)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	return v, nil
}

// AnsibleCore reports the ansible-core version installed on this server, or
// roundhouse.ErrAnsibleMissing, for doctor.
func (d *Dispatcher) AnsibleCore(ctx context.Context) (string, error) {
	if d.ansibleCoreReporter == nil {
		return "", roundhouse.ErrAnsibleMissing
	}
	return d.ansibleCoreReporter.AnsibleCoreVersion(ctx)
}

// readInputs resolves every input with the engine its content calls for: natively when it is a
// self-contained static document, and with ansible-inventory when only Ansible can read it. It
// returns the ansible-core version when Ansible read any input.
func (d *Dispatcher) readInputs(ctx context.Context, inputs []*composeInput) (string, error) {
	var (
		viaAnsible []*composeInput
		why        string
	)
	for _, in := range inputs {
		l, err := inventory.ResolveNative(in.content)
		if reason, ok := inventory.NeedsAnsibleReason(err); ok {
			in.engine = inventory.EngineAnsible
			if why == "" {
				why = reason
			}
			viaAnsible = append(viaAnsible, in)
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%w: input %s: %w", inventory.ErrResolve, in.inv.Name, err)
		}
		in.engine, in.listing = inventory.EngineNative, l
	}
	if len(viaAnsible) == 0 {
		return "", nil
	}
	version, err := d.ansibleCore(ctx, "input inventory "+quoted(viaAnsible[0].inv.Name), why)
	if err != nil {
		return "", err
	}
	errs := make([]error, len(viaAnsible))
	sem := make(chan struct{}, composeWorkers)
	var wg sync.WaitGroup
	for n, in := range viaAnsible {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			errs[n] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func(n int, in *composeInput) {
			defer wg.Done()
			defer func() { <-sem }()
			out, err := d.invLister.ListInventory(ctx, []string{in.path}, "")
			if err != nil {
				errs[n] = fmt.Errorf("%w: input %s: %w", inventory.ErrResolve, in.inv.Name, err)
				return
			}
			in.listing, errs[n] = inventory.ParseListing(out)
		}(n, in)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return "", err
	}
	return version, nil
}

// quoted wraps a name in double quotes for a message.
func quoted(name string) string { return `"` + name + `"` }

// composeSmart reads each input and keeps the hosts the filter selects, with their resolved
// variables. A host name that appears in more than one input is taken from the first, since a smart
// inventory, like AWX's, holds each host name once. It returns the listing, the inputs that
// contributed at least one host, and the ansible-core version when Ansible read any input.
func (d *Dispatcher) composeSmart(ctx context.Context, inv *inventory.Inventory,
	inputs []*composeInput) (*inventory.Listing, []string, string, error) {
	filter, err := inventory.ParseHostFilter(inv.HostFilter)
	if err != nil {
		return nil, nil, "", err
	}
	var facts map[string]map[string]string
	if filter.UsesFacts() {
		if facts, err = d.estateFacts(ctx); err != nil {
			return nil, nil, "", err
		}
	}
	version, err := d.readInputs(ctx, inputs)
	if err != nil {
		return nil, nil, "", err
	}
	seen := map[string]bool{}
	vars := map[string]map[string]any{}
	var hosts, drawn []string
	for _, in := range inputs {
		contributed := false
		for _, h := range in.listing.Hosts() {
			if seen[h] {
				continue
			}
			c := &inventory.Candidate{
				Name: h, Groups: in.listing.Groups(h), Vars: in.listing.Vars(h), Facts: facts[h],
				InventoryID: in.inv.ID, InventoryName: in.inv.Name,
			}
			if !filter.Match(c) {
				continue
			}
			seen[h] = true
			hosts = append(hosts, h)
			vars[h] = c.Vars
			contributed = true
		}
		if contributed {
			drawn = append(drawn, in.inv.ID)
		}
	}
	slices.Sort(hosts)
	return inventory.NewHostListing(hosts, vars), drawn, version, nil
}

// composeConstructed runs the constructed plugin over every input at once, the way AWX runs a
// constructed inventory update, and applies the limit after the groups are built so the limit may
// name a group the options construct. Every input read is recorded as drawn from. A constructed
// inventory is always resolved by Ansible: its options are Jinja expressions, which SwitchTender
// runs through Ansible rather than reimplements.
func (d *Dispatcher) composeConstructed(ctx context.Context, inv *inventory.Inventory,
	inputs []*composeInput, dir string) (*inventory.Listing, []string, string, error) {
	if len(inputs) == 0 {
		return inventory.NewHostListing(nil, nil), nil, "", nil
	}
	config, err := inventory.ConstructedConfig(inv.SourceVars)
	if err != nil {
		return nil, nil, "", err
	}
	version, err := d.ansibleCore(ctx, "constructed inventory "+quoted(inv.Name),
		"its groups and variables are Jinja expressions evaluated by Ansible's constructed plugin, "+
			"which SwitchTender runs rather than reimplements")
	if err != nil {
		return nil, nil, "", err
	}
	for _, in := range inputs {
		in.engine = inventory.EngineAnsible
	}
	// The plugin is chosen by the file's extension and its plugin key, so the config is written as
	// YAML and read last, after every input it builds groups from.
	path := filepath.Join(dir, "constructed.yml")
	if err := os.WriteFile(path, config, 0o600); err != nil {
		return nil, nil, "", fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	sources := make([]string, 0, len(inputs)+1)
	drawn := make([]string, 0, len(inputs))
	for _, in := range inputs {
		sources = append(sources, in.path)
		drawn = append(drawn, in.inv.ID)
	}
	sources = append(sources, path)
	out, err := d.invLister.ListInventory(ctx, sources, inv.Limit)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%w: %s: %w", inventory.ErrResolve, inv.Name, err)
	}
	listing, err := inventory.ParseListing(out)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%w: %s: %w", inventory.ErrResolve, inv.Name, err)
	}
	return listing, drawn, version, nil
}

// estateFacts returns the facts last gathered for every host, keyed by host name.
func (d *Dispatcher) estateFacts(ctx context.Context) (map[string]map[string]string, error) {
	estate, err := d.store.EstateAt(ctx, d.now(), 0)
	if err != nil {
		return nil, fmt.Errorf("%w: read gathered facts: %w", inventory.ErrResolve, err)
	}
	out := make(map[string]map[string]string, len(estate))
	for _, f := range estate {
		out[f.Host] = f.Facts
	}
	return out, nil
}

// maskComposeError removes the secret-looking values of the inputs from a resolution error. Ansible
// quotes the line it could not parse, and an inventory line routinely carries a password.
func maskComposeError(err error, secrets []string) error {
	msg := err.Error()
	masked, _ := util.RedactAssignments(msg, inventory.RedactedValue)
	for _, s := range secrets {
		if len(s) >= 4 {
			masked = strings.ReplaceAll(masked, s, inventory.RedactedValue)
		}
	}
	if masked == msg {
		return err
	}
	return &maskedError{msg: masked, cause: err}
}

// maskedError carries a redacted message while still matching the sentinel it wraps.
type maskedError struct {
	// msg is the redacted message.
	msg string
	// cause is the original error, kept for errors.Is and never printed.
	cause error
}

// Error returns the redacted message.
func (e *maskedError) Error() string { return e.msg }

// Unwrap returns the original error so errors.Is still finds the sentinel.
func (e *maskedError) Unwrap() error { return e.cause }
