// Package template holds job templates: saved launch presets that bundle a project, playbook,
// inventory, shards, credentials, and extra vars so a run launches in one action instead of a
// hand-built request.
package template

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// ErrNotFound is returned when a template does not exist in the store.
var ErrNotFound = errors.New("template not found")

// Template is one saved launch preset.
type Template struct {
	// ID is the unique template identifier.
	ID string `json:"id"`
	// Name labels the template for humans, for example deploy production.
	Name string `json:"name"`
	// ProjectID sources the playbook from a git project. Empty for local paths.
	ProjectID string `json:"project_id,omitempty"`
	// Playbook is the playbook path, relative to the project when one is set.
	Playbook string `json:"playbook"`
	// Inventory is the inventory path, relative to the project when one is set.
	Inventory string `json:"inventory,omitempty"`
	// Shards, when two or more, splits the run across that many inventory slices.
	Shards int `json:"shards,omitempty"`
	// CredentialIDs names stored credentials materialized for the run.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ExtraVars are injected into the run as extra vars.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// CreatedAt is when the template was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists templates. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the template identified by t.ID.
	Save(ctx context.Context, t *Template) error
	// Get returns the template with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Template, error)
	// List returns all templates ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Template, error)
	// Delete removes the template with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random template identifier prefixed with "tpl_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("template: read random: " + err.Error())
	}
	return "tpl_" + hex.EncodeToString(b[:])
}
