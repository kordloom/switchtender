package server

import (
	"context"
	"errors"

	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/team"
	"github.com/dcadolph/yardmaster/internal/user"
)

// errForbiddenGrant is returned by authorize when the actor lacks a grant the object requires.
var errForbiddenGrant = errors.New("forbidden: no grant for this object")

// authorizer decides object-level access on top of the coarse global role. It is additive: an
// object with no grants defers to the role, so existing installs behave as before, unless strict
// grants are on, which flips unlisted objects to deny for non-admins.
type authorizer struct {
	// grants stores per-object access grants; nil disables object-level checks entirely.
	grants grant.Store
	// teams resolves an actor's team memberships so team grants apply to their members.
	teams team.Store
	// strict makes an object with no grants deny non-admins instead of deferring to the role.
	strict bool
}

// authorize reports whether the request's actor may exercise want access on object. Admins and
// command-line tokens bypass. An object with no grants defers to the global role, unless strict
// grants are enabled. Otherwise a grant to the actor or one of their teams that satisfies want is
// required. It returns errForbiddenGrant when access is denied.
func (a *authorizer) authorize(ctx context.Context, object string, want grant.Access) error {
	if a == nil || a.grants == nil {
		return nil
	}
	actor, ok := actorFrom(ctx)
	if !ok {
		return nil
	}
	if actor.Role == user.RoleAdmin {
		return nil
	}

	grants, err := a.grants.ForObject(ctx, object)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		if a.strict {
			return errForbiddenGrant
		}
		return nil
	}

	subjects, err := a.subjectsFor(ctx, actor)
	if err != nil {
		return err
	}
	for _, g := range grants {
		if subjects[g.Subject] && grant.Satisfies(g.Access, want) {
			return nil
		}
	}
	return errForbiddenGrant
}

// subjectsFor returns the set of grant subject ids that represent the actor: their own user id and
// every team they belong to.
func (a *authorizer) subjectsFor(ctx context.Context, actor Actor) (map[string]bool, error) {
	subjects := map[string]bool{}
	if actor.UserID == "" {
		return subjects, nil
	}
	subjects[actor.UserID] = true
	if a.teams == nil {
		return subjects, nil
	}
	teamIDs, err := a.teams.TeamsForUser(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	for _, tid := range teamIDs {
		subjects[tid] = true
	}
	return subjects, nil
}
