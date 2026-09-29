// Package grant records per-object access grants so a large organization can scope who may use or
// manage a specific project, template, inventory, or credential beyond the coarse global role. A
// grant ties a subject, a user, a team, or an organization, to an object with an access level. Grants are additive:
// when an object has no grants the global role decides.
package grant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned when a grant does not exist in the store.
var ErrNotFound = errors.New("grant not found")

// Access is the level a grant confers.
type Access string

const (
	// AccessRead lets a subject see an object in a listing without using or changing it. It is the
	// lowest level, so a use or manage grant confers it too.
	AccessRead Access = "read"
	// AccessUse lets a subject use an object, for example launch a template or run a project, and
	// implies read.
	AccessUse Access = "use"
	// AccessManage lets a subject change an object and implies use and read.
	AccessManage Access = "manage"
)

// subjectPrefixes are the id prefixes a grant subject may carry.
var subjectPrefixes = []string{"user_", "team_", "org_"}

// objectPrefixes are the id prefixes a grant object may carry.
var objectPrefixes = []string{"proj_", "tpl_", "inv_", "cred_"}

// Grant ties a subject to an object at an access level.
type Grant struct {
	// ID is the unique grant identifier.
	ID string `json:"id"`
	// Subject is the granted identity: a user id (user_...), a team id (team_...), or an org id (org_...).
	Subject string `json:"subject"`
	// Object is the target: a project, template, inventory, or credential id.
	Object string `json:"object"`
	// Access is the level conferred: read, use, or manage, each implying the ones below it.
	Access Access `json:"access"`
	// CreatedAt is when the grant was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists grants. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the grant identified by g.ID.
	Save(ctx context.Context, g *Grant) error
	// Get returns the grant with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Grant, error)
	// List returns all grants ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Grant, error)
	// Delete removes the grant with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// ForObject returns every grant on the given object.
	ForObject(ctx context.Context, object string) ([]*Grant, error)
}

// ValidAccess reports whether a names a supported access level.
func ValidAccess(a Access) bool {
	return a == AccessRead || a == AccessUse || a == AccessManage
}

// rank orders the access levels so a higher level confers the lower ones: manage over use over read.
// An unrecognized level ranks zero, so it confers nothing.
func rank(a Access) int {
	switch a {
	case AccessManage:
		return 3
	case AccessUse:
		return 2
	case AccessRead:
		return 1
	default:
		return 0
	}
}

// Satisfies reports whether a subject holding have may perform an action needing want. The levels are
// ranked, so manage satisfies use and read, and use satisfies read.
func Satisfies(have, want Access) bool {
	return rank(have) >= rank(want)
}

// ValidSubject reports whether s names a user or a team.
func ValidSubject(s string) bool {
	return hasAnyPrefix(s, subjectPrefixes)
}

// ValidObject reports whether o names a grantable object.
func ValidObject(o string) bool {
	return hasAnyPrefix(o, objectPrefixes)
}

// hasAnyPrefix reports whether s starts with any of the prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// NewID returns a random grant identifier prefixed with "grant_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("grant: read random: " + err.Error())
	}
	return "grant_" + hex.EncodeToString(b[:])
}
