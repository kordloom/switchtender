// Package team groups users so per-object grants can be handed to a whole team at once. A team is
// a named set of user accounts; membership is a many-to-many link resolved when authorizing an
// object access.
package team

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// ErrNotFound is returned when a team does not exist in the store.
var ErrNotFound = errors.New("team not found")

// Team is a named group of users.
type Team struct {
	// ID is the unique team identifier.
	ID string `json:"id"`
	// Name labels the team for humans.
	Name string `json:"name"`
	// CreatedAt is when the team was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists teams and their memberships. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the team identified by t.ID.
	Save(ctx context.Context, t *Team) error
	// Get returns the team with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Team, error)
	// List returns all teams ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Team, error)
	// Delete removes the team with the given id and its memberships, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// AddMember adds a user to a team. Adding an existing member is a no-op.
	AddMember(ctx context.Context, teamID, userID string) error
	// RemoveMember removes a user from a team. Removing a non-member is a no-op.
	RemoveMember(ctx context.Context, teamID, userID string) error
	// Members returns the user ids in a team, sorted.
	Members(ctx context.Context, teamID string) ([]string, error)
	// TeamsForUser returns the ids of the teams a user belongs to, sorted.
	TeamsForUser(ctx context.Context, userID string) ([]string, error)
}

// NewID returns a random team identifier prefixed with "team_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("team: read random: " + err.Error())
	}
	return "team_" + hex.EncodeToString(b[:])
}
