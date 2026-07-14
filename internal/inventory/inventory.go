// Package inventory stores named Ansible inventories so runs reference them by id instead of a
// file path that must exist on every executor. The content materializes to a file at execution
// time on whichever process runs the play.
package inventory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// ErrNotFound is returned when an inventory does not exist in the store.
var ErrNotFound = errors.New("inventory not found")

// Inventory is one stored inventory document, INI or YAML, exactly as Ansible reads it.
type Inventory struct {
	// ID is the unique inventory identifier.
	ID string `json:"id"`
	// Name labels the inventory for humans, for example production fleet.
	Name string `json:"name"`
	// Content is the inventory text.
	Content string `json:"content"`
	// CredentialIDs names stored credentials materialized for every run that targets this inventory,
	// so an inventory can carry its own secret variables, for example an env credential of cloud
	// keys the plugin needs.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ContentSource selects where the content comes from at run time: local (the stored Content),
	// command, vault, or gsm. Empty means local. When it is not local, ContentConfig is resolved at
	// launch and used as the content, so the host list can live in Vault, Google Secret Manager, or
	// behind a command rather than in Railwarden.
	ContentSource string `json:"content_source,omitempty"`
	// ContentConfig is the source config for a non-local ContentSource, sealed at rest and never
	// returned by the API. It is the command for command, or the JSON address, path, and field for
	// vault, or project, secret, and version for gsm.
	ContentConfig string `json:"-"`
	// Queue pins every run that targets this inventory to workers serving the queue, unless the
	// run or its template names a queue of its own. Empty uses the default pool.
	Queue string `json:"queue,omitempty"`
	// CreatedAt is when the inventory was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists inventories. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the inventory identified by i.ID.
	Save(ctx context.Context, i *Inventory) error
	// Update changes an existing inventory's name and content, preserving its creation time, or
	// returns ErrNotFound.
	Update(ctx context.Context, i *Inventory) error
	// Get returns the inventory with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Inventory, error)
	// List returns all inventories ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Inventory, error)
	// Delete removes the inventory with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random inventory identifier prefixed with "inv_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("inventory: read random: " + err.Error())
	}
	return "inv_" + hex.EncodeToString(b[:])
}
