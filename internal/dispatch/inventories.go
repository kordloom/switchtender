package dispatch

import (
	"context"
	"fmt"
	"os"
	"regexp"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// WithInventories lets runs target stored inventories by id.
func WithInventories(store inventory.Store) Option {
	return func(c *config) { c.inventories = store }
}

// validateInventory confirms a referenced inventory exists before a run is accepted.
func (d *Dispatcher) validateInventory(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if d.inventories == nil {
		return inventory.ErrNotFound
	}
	if _, err := d.inventories.Get(ctx, id); err != nil {
		return fmt.Errorf("%w: %s", err, id)
	}
	return nil
}

// materializeInventory writes the run's stored inventory to a file for the executor and points the
// spec at it. It returns the cleanup that removes the file and the secret-looking values in the host
// list, so a variable such as ansible_password is masked in the run's output.
func (d *Dispatcher) materializeInventory(ctx context.Context, r *run.Run, spec *roundhouse.Spec) (func(), []string, error) {
	cleanup := func() {}
	if r.InventoryID == "" {
		return cleanup, nil, nil
	}
	path, remove, secrets, err := d.inventoryFile(ctx, r.InventoryID)
	if err != nil {
		return cleanup, nil, err
	}
	spec.Inventory = path
	return remove, secrets, nil
}

// inventoryFile materializes a stored inventory to a temp file and returns its path, cleanup, and the
// secret-looking values in its content.
func (d *Dispatcher) inventoryFile(ctx context.Context, id string) (string, func(), []string, error) {
	if d.inventories == nil {
		return "", func() {}, nil, inventory.ErrNotFound
	}
	inv, err := d.inventories.Get(ctx, id)
	if err != nil {
		return "", func() {}, nil, fmt.Errorf("inventory %s: %w", id, err)
	}
	content, err := d.inventoryContent(ctx, inv)
	if err != nil {
		return "", func() {}, nil, err
	}
	f, err := os.CreateTemp("", "switchtender-inventory-*")
	if err != nil {
		return "", func() {}, nil, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, nil, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, nil, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, inventorySecrets(content), nil
}

// inventoryContent returns the inventory's content, resolving it from its content source when that
// source is not local. A non-local source's config is sealed, so it is decrypted and then resolved
// through the shared secretsource engine, letting the host list live in Vault, Google Secret Manager,
// or behind a command rather than in SwitchTender.
func (d *Dispatcher) inventoryContent(ctx context.Context, inv *inventory.Inventory) (string, error) {
	if secretsource.NormalizeKind(inv.ContentSource) == secretsource.KindLocal {
		return inv.Content, nil
	}
	if d.sealer == nil {
		return "", fmt.Errorf("inventory %s content source needs an encryption key", inv.ID)
	}
	config, err := d.sealer.Open(inv.ContentConfig)
	if err != nil {
		return "", fmt.Errorf("inventory %s decrypt content source: %w", inv.ID, err)
	}
	content, err := secretsource.Resolve(ctx, inv.ContentSource, config)
	if err != nil {
		return "", fmt.Errorf("inventory %s resolve content: %w", inv.ID, err)
	}
	return content, nil
}

// inventorySecretPattern matches an inventory variable assignment whose name suggests a secret, in
// the ini form key=value or the yaml form key: value. The captured group is the value, which is
// either a quoted run (spaces allowed, up to the closing quote) or an unquoted run up to the next
// whitespace. The quoted branch is what lets a password with spaces be masked in full; the
// unquoted branch stops at whitespace so a second variable on the same INI host line is still
// matched on the next pass rather than swallowed. The password, passwd, passphrase, secret, token,
// and api_key stems may carry trailing name parts, so secret_value and token_id still match. The
// bare pass stem is deliberately narrower: it matches only as a terminal _pass component, which
// catches ansible_ssh_pass and ansible_become_pass without swallowing bypass, passive, or
// passthrough, whose values are often booleans the masker would then black out everywhere in a
// run's output. Key-file paths and access-key IDs are not secret and are left unmatched.
var inventorySecretPattern = regexp.MustCompile(
	`(?i)[a-z0-9_]*(?:(?:password|passwd|passphrase|secret|token|api[_-]?key)[a-z0-9_]*|_pass)` +
		`\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s]+)`)

// inventorySecrets returns the values of secret-looking variables in inventory content, so a host list
// that carries an ansible_password or an API token does not leak it into the run's log or events.
// A quoted value is unwrapped so the masker holds the bare secret and matches it literally in output;
// capturing the surrounding quotes would mask a string the output never contains.
func inventorySecrets(content string) []string {
	var out []string
	for _, m := range inventorySecretPattern.FindAllStringSubmatch(content, -1) {
		v := m[1]
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
