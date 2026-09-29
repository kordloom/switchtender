// Package invsource stores dynamic inventory sources: a plugin config or script that produces
// hosts at refresh time. Refreshing a source runs ansible-inventory against it and writes the
// result into a stored inventory, so runs target the refreshed hosts by the ordinary inventory
// id and every downstream feature works unchanged.
package invsource

import (
	"context"
	"errors"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
)

// ErrNotFound is returned when a source does not exist in the store.
var ErrNotFound = errors.New("inventory source not found")

// ErrInvalidSource is returned when a source path is unsafe to hand to ansible-inventory, such as an
// executable file or a path that traverses out of its intended location.
var ErrInvalidSource = errors.New("invalid inventory source")

// Source is one dynamic inventory source.
type Source struct {
	// ID is the unique source identifier.
	ID string `json:"id"`
	// Name labels the source for humans.
	Name string `json:"name"`
	// Source is the ansible-inventory argument: an inventory plugin config file, a script, or a
	// directory. Relative to the project checkout when ProjectID is set.
	Source string `json:"source"`
	// CredentialID names an env credential whose variables authenticate the plugin. Optional.
	CredentialID string `json:"credential_id,omitempty"`
	// ProjectID sources the config from a git project. Optional.
	ProjectID string `json:"project_id,omitempty"`
	// InventoryID is the stored inventory this source maintains; it is created with the source.
	InventoryID string `json:"inventory_id"`
	// SyncedAt is when the source last refreshed successfully.
	SyncedAt *time.Time `json:"synced_at,omitempty"`
	// LastError holds the most recent refresh failure, empty on success.
	LastError string `json:"last_error,omitempty"`
	// UpdateOnLaunch refreshes the source before a run targeting its backing inventory, so a launch
	// sees current hosts rather than the last synced snapshot.
	UpdateOnLaunch bool `json:"update_on_launch,omitempty"`
	// SyncIntervalSeconds is how often a background sync refreshes the source, and the staleness
	// window an update-on-launch refresh honors. Zero disables scheduled sync and refreshes on every
	// launch.
	SyncIntervalSeconds int `json:"sync_interval_seconds,omitempty"`
	// CreatedAt is when the source was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists inventory sources. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the source identified by s.ID.
	Save(ctx context.Context, s *Source) error
	// Update changes an existing source's name, source, and referenced credential and project,
	// leaving its backing inventory and sync state intact, or returns ErrNotFound.
	Update(ctx context.Context, s *Source) error
	// Get returns the source with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Source, error)
	// List returns all sources ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Source, error)
	// Delete removes the source with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random source identifier prefixed with "src_".
func NewID() string {
	return idgen.New("src_", 6)
}
