package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
)

// plainPrefix marks sealed material stored by an install that holds no encryption key. Such an
// install keeps its inventories and everything else in plain text at rest already, so its snapshots
// and plan files are stored the same way, encoded but not encrypted. An install that holds a key
// never writes one and refuses to open one, so a plain value cannot stand in for a sealed one.
const plainPrefix = "plain:"

// errStoredPlain is why a plain value is refused on an install that holds a key.
var errStoredPlain = errors.New("it is stored unsealed on an install that holds an encryption key")

// sealBytes seals b for storage on a run with the server's key, or encodes it as plain on an install
// that holds none.
func (d *Dispatcher) sealBytes(b []byte) (string, error) {
	encoded := base64.StdEncoding.EncodeToString(b)
	if d.sealer == nil || !d.sealer.Enabled() {
		return plainPrefix + encoded, nil
	}
	return d.sealer.Seal(encoded)
}

// openBytes opens a value sealBytes stored. A value stored plain opens only on an install with no
// key, and a sealed value only on one with the key that sealed it.
func (d *Dispatcher) openBytes(sealed string) ([]byte, error) {
	keyed := d.sealer != nil && d.sealer.Enabled()
	if rest, ok := strings.CutPrefix(sealed, plainPrefix); ok {
		if keyed {
			return nil, errStoredPlain
		}
		return base64.StdEncoding.DecodeString(rest)
	}
	if !keyed {
		return nil, credential.ErrNoKey
	}
	encoded, err := d.sealer.Open(sealed)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(encoded)
}

// maskedContentSHA256 returns the hex SHA-256 of inventory content with every value the inventory
// redactor treats as secret masked, which is exactly the content the inventory API serves. A reader
// holding the inventory can recompute it, and it reveals nothing that reader could not already see.
func maskedContentSHA256(content string) string {
	sum := sha256.Sum256([]byte(inventory.Redact(content)))
	return hex.EncodeToString(sum[:])
}

// snapshotOf reads what a snapshot record says about inventory content: the masked digest, and the
// hosts it names when the native engine resolves it, or that it is a dynamic source whose hosts
// exist only once Ansible resolves it when the run executes. Content Ansible would not read at all
// is refused here, at submission, rather than discovered when the run executes.
func snapshotOf(content string) (*run.InventorySnapshot, error) {
	snap := &run.InventorySnapshot{ContentSHA256: maskedContentSHA256(content)}
	hosts, err := inventory.NativeHosts(content)
	if reason, ok := inventory.NeedsAnsibleReason(err); ok {
		snap.Dynamic, snap.DynamicReason = true, reason
		return snap, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	snap.Hosts = names
	return snap, nil
}

// snapshotInventory materializes the plain stored inventory r targets, once, when r is submitted,
// and records the snapshot on r: the content sealed, and the record an approval binds. The run then
// executes this snapshot and never whatever the store holds when it is claimed, so an inventory
// edited after an approval cannot widen or redirect the approved run, and an inventory edited
// between a submission and its claim cannot either.
//
// A composed inventory took its snapshot when it was resolved, unless the run arrives held to a
// resolution and carrying no snapshot, which snapshotHeldComposed covers. A run derived from
// another, a shard of a split, a step of a workflow, or the apply a plan proposes, arrives carrying
// its source's snapshot and keeps it: it is part of the same submission, so it executes the same
// inventory.
func (d *Dispatcher) snapshotInventory(ctx context.Context, r *run.Run) error {
	if r.InventorySnapshot != nil && r.InventorySealed != "" {
		return nil
	}
	r.InventorySnapshot, r.InventorySealed = nil, ""
	if r.InventoryID == "" || d.inventories == nil {
		return nil
	}
	inv, err := d.inventories.Get(ctx, r.InventoryID)
	if err != nil {
		return fmt.Errorf("%w: %s", err, r.InventoryID)
	}
	if inv.Composed() {
		return d.snapshotHeldComposed(ctx, r, inv)
	}
	// An inventory that refreshes from its source on launch refreshes now, when the run is submitted,
	// so the snapshot taken next, the one an approver sees and the run executes, holds the refreshed
	// hosts. A refresh at execution would refresh nothing this run reads.
	if d.refreshOnLaunch(ctx, r) {
		if inv, err = d.inventories.Get(ctx, r.InventoryID); err != nil {
			return fmt.Errorf("%w: %s", err, r.InventoryID)
		}
	}
	content, err := d.inventoryContent(ctx, inv)
	if err != nil {
		return fmt.Errorf("%w: inventory %s could not be read when the run was submitted: %w",
			ErrInventorySnapshot, inv.Name, err)
	}
	snap, err := snapshotOf(content)
	if err != nil {
		return fmt.Errorf("inventory %s: %w", inv.Name, err)
	}
	sealed, err := d.sealBytes([]byte(content))
	if err != nil {
		return fmt.Errorf("%w: seal the snapshot of inventory %s: %w", ErrInventorySnapshot,
			inv.Name, err)
	}
	snap.SealedSHA256 = run.SealedBlobSHA256(sealed)
	snap.CredentialIDs = slices.Clone(inv.CredentialIDs)
	r.InventorySnapshot, r.InventorySealed = snap, sealed
	return nil
}

// composedSnapshot is what a composed inventory's snapshot seals: the result a run executes and the
// content of every input it drew hosts from, each as it was read when the run was submitted. The
// inputs are kept so the executor still checks Ansible's reading of each one against the native
// engine's, without reading any input again.
type composedSnapshot struct {
	// Content is the composed result, the inventory handed to the play.
	Content string `json:"content"`
	// Inputs are the inputs the hosts were drawn from, in the order the resolution names them.
	Inputs []snapshotInput `json:"inputs"`
}

// snapshotInput is one input inventory as a composed snapshot holds it.
type snapshotInput struct {
	// ID names the input inventory.
	ID string `json:"id"`
	// Name is the input's name when the run was submitted, for the messages that name it.
	Name string `json:"name"`
	// Content is the input's content, its variables and secrets included, as it was read.
	Content string `json:"content"`
	// Engine is the engine that read the input when it was composed, native or ansible. Only an
	// input the native engine read is checked against Ansible's reading before an Ansible run.
	Engine string `json:"engine"`
}

// snapshotComposed seals what the composed inventory inv resolved to for r, c, as r's inventory
// snapshot, through the same snapshot record a plain inventory takes: the composed result's hosts,
// its digest with secrets masked, the digest of the sealed content, and the credentials inv
// attaches. The sealed content holds the result and every input it drew from, so the run executes
// exactly what was resolved and approved, and an input edited afterward, a variable included, does
// not reach it.
func (d *Dispatcher) snapshotComposed(r *run.Run, inv *inventory.Inventory, c *composed) error {
	snap, err := snapshotOf(c.Content)
	if err != nil {
		return fmt.Errorf("%w: composed inventory %s: %w", ErrInventorySnapshot, inv.Name, err)
	}
	if snap.Dynamic {
		return fmt.Errorf("%w: composed inventory %s resolved to content the native engine cannot read",
			ErrInventorySnapshot, inv.Name)
	}
	byID := make(map[string]*composeInput, len(c.inputs))
	for _, in := range c.inputs {
		byID[in.inv.ID] = in
	}
	body := composedSnapshot{Content: c.Content}
	for _, id := range c.Resolution.Inputs {
		if in := byID[id]; in != nil {
			body.Inputs = append(body.Inputs, snapshotInput{ID: id, Name: in.inv.Name,
				Content: in.content, Engine: in.engine})
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: composed inventory %s: %w", ErrInventorySnapshot, inv.Name, err)
	}
	defer clear(raw)
	sealed, err := d.sealBytes(raw)
	if err != nil {
		return fmt.Errorf("%w: seal the snapshot of composed inventory %s: %w", ErrInventorySnapshot,
			inv.Name, err)
	}
	snap.SealedSHA256 = run.SealedBlobSHA256(sealed)
	snap.CredentialIDs = slices.Clone(inv.CredentialIDs)
	r.InventorySnapshot, r.InventorySealed = snap, sealed
	return nil
}

// snapshotHeldComposed snapshots the composed inventory inv for a run that arrives held to a
// resolution and carrying no snapshot: the retry of a split's failed shards, whose parent's
// snapshot was wiped when the parent ended. The retry is a submission of its own, so the inventory
// is composed again now, from its inputs as they stand, held to the hosts the resolution recorded,
// and that is what the retry records and executes. A run with no resolution takes its snapshot when
// resolveComposed resolves it.
func (d *Dispatcher) snapshotHeldComposed(ctx context.Context, r *run.Run, inv *inventory.Inventory) error {
	if r.InventoryResolution == nil {
		return nil
	}
	c, err := d.composedContent(ctx, inv, r.InventoryResolution)
	if err != nil {
		return err
	}
	r.InventoryResolution = c.Resolution
	return d.snapshotComposed(r, inv, c)
}

// parseComposedSnapshot decodes opened composed snapshot content.
func parseComposedSnapshot(content string) (*composedSnapshot, error) {
	var out composedSnapshot
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("%w: the opened composed inventory snapshot does not read: %w",
			ErrInventorySnapshot, err)
	}
	return &out, nil
}

// snapshotInputDigest returns the digest of a composed snapshot's inputs, the form the resolution's
// input digest takes: each input by id and its content with secrets masked, in order.
func snapshotInputDigest(inputs []snapshotInput) string {
	list := make([]inventory.DigestInput, 0, len(inputs))
	for _, in := range inputs {
		list = append(list, inventory.DigestInput{ID: in.ID, Content: in.Content})
	}
	return inventory.InputDigest(list)
}

// snapshotPlay returns the inventory content the play is handed from opened snapshot content: the
// content itself for a plain inventory, the composed result for a composed one.
func snapshotPlay(r *run.Run, content string) (string, error) {
	if r.InventoryResolution == nil {
		return content, nil
	}
	cs, err := parseComposedSnapshot(content)
	if err != nil {
		return "", err
	}
	return cs.Content, nil
}

// composedFromSnapshot rebuilds what the cross-check before an Ansible run reads from opened
// composed snapshot content: the result handed to the play, read by the native engine, and every
// input with the engine that read it when it was composed. Nothing is read from the inventory
// store.
func composedFromSnapshot(r *run.Run, content string) (*composed, error) {
	cs, err := parseComposedSnapshot(content)
	if err != nil {
		return nil, err
	}
	listing, err := inventory.ResolveNative(cs.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: the composed inventory snapshot: %w", ErrInventorySnapshot, err)
	}
	inputs := make([]*composeInput, 0, len(cs.Inputs))
	for _, si := range cs.Inputs {
		in := &composeInput{inv: &inventory.Inventory{ID: si.ID, Name: si.Name}, content: si.Content,
			engine: si.Engine}
		if si.Engine == inventory.EngineNative {
			if in.listing, err = inventory.ResolveNative(si.Content); err != nil {
				return nil, fmt.Errorf("%w: input %s in the snapshot: %w", ErrInventorySnapshot, si.Name,
					err)
			}
		}
		inputs = append(inputs, in)
	}
	return &composed{
		Composition: &Composition{Resolution: r.InventoryResolution, Content: cs.Content},
		listing:     listing, inputs: inputs,
	}, nil
}

// openSnapshot opens the sealed inventory snapshot r was submitted with, after checking that it is
// the snapshot r's record binds. A run that carries none, one whose sealed content no longer
// matches the digest bound when it was submitted, and one that does not open are all refused.
func (d *Dispatcher) openSnapshot(r *run.Run) (string, error) {
	switch {
	case r.InventorySealed == "":
		return "", fmt.Errorf("%w: the inventory snapshot this run was submitted with is missing, "+
			"so it is refused rather than run against whatever the inventory holds now", ErrInventorySnapshot)
	case !r.SnapshotMatches(r.InventorySealed):
		return "", fmt.Errorf("%w: the stored inventory snapshot changed after the run was "+
			"submitted, so this is not the inventory its approval covers", ErrInventorySnapshot)
	}
	b, err := d.openBytes(r.InventorySealed)
	if err != nil {
		return "", fmt.Errorf("%w: the inventory snapshot does not open: %w", ErrInventorySnapshot,
			err)
	}
	content := string(b)
	if err := checkSnapshotContent(r, content); err != nil {
		return "", err
	}
	return content, nil
}

// checkSnapshotContent holds opened snapshot content to the masked digest r's record binds, and for
// a composed inventory its inputs to the input digest r's resolution binds. It is the check a relay
// worker can make itself, since the sealed form the other digest covers never leaves the control
// node.
func checkSnapshotContent(r *run.Run, content string) error {
	if r.InventorySnapshot == nil {
		return fmt.Errorf("%w: the opened inventory snapshot is not the content this run was "+
			"submitted with", ErrInventorySnapshot)
	}
	play := content
	if r.InventoryResolution != nil {
		cs, err := parseComposedSnapshot(content)
		if err != nil {
			return err
		}
		if snapshotInputDigest(cs.Inputs) != r.InventoryResolution.InputDigest {
			return fmt.Errorf("%w: the inputs in the opened composed inventory snapshot are not the "+
				"ones this run resolved from", ErrInventorySnapshot)
		}
		play = cs.Content
	}
	if maskedContentSHA256(play) != r.InventorySnapshot.ContentSHA256 {
		return fmt.Errorf("%w: the opened inventory snapshot is not the content this run was "+
			"submitted with", ErrInventorySnapshot)
	}
	return nil
}

// materializeSnapshot writes the opened snapshot content to a private file for the run and points
// spec at it, returning the cleanup that removes it and the secret values the content carries.
func (d *Dispatcher) materializeSnapshot(r *run.Run, content string,
	spec *roundhouse.Spec) (func(), []string, error) {
	dir, err := runfiles.Create(d.runFiles(), "inventory-"+r.ID)
	if err != nil {
		return func() {}, nil, fmt.Errorf("materialize the inventory snapshot: %w", err)
	}
	remove := func() { _ = dir.Remove() }
	path := filepath.Join(dir.Path(), "inventory")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		remove()
		return func() {}, nil, fmt.Errorf("materialize the inventory snapshot: %w", err)
	}
	spec.Inventory = path
	return remove, inventorySecrets(content), nil
}

// resolveDynamicSnapshot resolves a dynamic inventory source once, as the run's own Ansible reads it,
// with the run's environment, project configuration, and image, records the hosts it resolved to on
// the run, and hands the play exactly that resolution. A dynamic source answers from live systems,
// so reading it twice could answer twice differently: resolving it once and running the play against
// the result is what makes the hosts the outcome records the hosts the play reached.
//
// It returns the secret values the resolution carries, which the definition did not, so the caller
// masks them. It refuses the run when the source cannot be read, since a run whose reach cannot be
// recorded is not one this executor runs.
func (d *Dispatcher) resolveDynamicSnapshot(ctx context.Context, r *run.Run, spec *roundhouse.Spec,
	mask func(string) string) ([]string, error) {
	if r.InventorySnapshot == nil || !r.InventorySnapshot.Dynamic ||
		run.NormalizeTool(r.Tool) != run.ToolAnsible {
		return nil, nil
	}
	if d.invReader == nil {
		return nil, fmt.Errorf("%w: %w: this run's inventory is a dynamic source, and this executor "+
			"cannot read it with Ansible to record the hosts it resolves to", ErrInventorySnapshot,
			inventory.ErrNeedsAnsible)
	}
	dir := filepath.Dir(spec.Inventory)
	outs, _, err := d.invReader.ReadInventories(ctx, *spec, dir, []string{filepath.Base(spec.Inventory)})
	if errors.Is(err, roundhouse.ErrAnsibleMissing) {
		return nil, fmt.Errorf("%w: %w: this run's inventory is a dynamic source, and ansible-inventory "+
			"is not installed on this executor. Install it with: %s", ErrInventorySnapshot,
			inventory.ErrNeedsAnsible, inventory.AnsibleInstallHint)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: the dynamic inventory source could not be resolved: %s",
			ErrInventorySnapshot, mask(err.Error()))
	}
	listing, err := inventory.ParseListing(outs[0])
	if err != nil {
		return nil, fmt.Errorf("%w: the dynamic inventory source: %w", ErrInventorySnapshot, err)
	}
	static, err := listing.Static()
	if err != nil {
		return nil, fmt.Errorf("%w: the dynamic inventory source: %w", ErrInventorySnapshot, err)
	}
	if err := os.WriteFile(spec.Inventory, static, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write the resolved inventory: %w", ErrInventorySnapshot, err)
	}
	r.ResolvedHosts = listing.Hosts()
	return inventorySecrets(string(static)), nil
}
