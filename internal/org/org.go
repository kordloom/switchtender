// Package org groups users into organizations so access can be granted to a whole organization at
// once and administered by its own admins. An organization is a named set of members, each with an
// organization role. A membership resolves to a grant subject when authorizing object access, so a
// grant to an organization reaches every member, the same way a team grant does.
package org

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// ErrNotFound is returned when an organization does not exist in the store.
var ErrNotFound = errors.New("organization not found")

// Role is a member's authority within an organization.
type Role string

const (
	// RoleAdmin may administer the organization: manage its membership and, as a grant subject, hold
	// whatever access is granted to the organization.
	RoleAdmin Role = "admin"
	// RoleMember belongs to the organization and holds whatever access is granted to it.
	RoleMember Role = "member"
)

// ValidRole reports whether r names a supported organization role.
func ValidRole(r Role) bool { return r == RoleAdmin || r == RoleMember }

// Org is a named organization.
type Org struct {
	// ID is the unique organization identifier.
	ID string `json:"id"`
	// Name labels the organization for humans.
	Name string `json:"name"`
	// CreatedAt is when the organization was created.
	CreatedAt time.Time `json:"created_at"`
}

// Member is a user's membership in an organization.
type Member struct {
	// UserID is the member's user id.
	UserID string `json:"user_id"`
	// Role is the member's authority within the organization.
	Role Role `json:"role"`
}

// Membership is an organization a user belongs to and the role they hold there.
type Membership struct {
	// OrgID is the organization's id.
	OrgID string `json:"org_id"`
	// Role is the user's authority within the organization.
	Role Role `json:"role"`
}

// Store persists organizations and their memberships. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the organization identified by o.ID.
	Save(ctx context.Context, o *Org) error
	// Get returns the organization with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Org, error)
	// List returns all organizations ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Org, error)
	// Delete removes the organization with the given id and its memberships, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// AddMember adds a user to an organization with a role, or updates the role of an existing member.
	AddMember(ctx context.Context, orgID, userID string, role Role) error
	// RemoveMember removes a user from an organization. Removing a non-member is a no-op.
	RemoveMember(ctx context.Context, orgID, userID string) error
	// Members returns an organization's members sorted by user id.
	Members(ctx context.Context, orgID string) ([]Member, error)
	// OrgsForUser returns the organizations a user belongs to, sorted by org id.
	OrgsForUser(ctx context.Context, userID string) ([]Membership, error)
}

// NewID returns a random organization identifier prefixed with "org_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("org: read random: " + err.Error())
	}
	return "org_" + hex.EncodeToString(b[:])
}
