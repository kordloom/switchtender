package server

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/run"
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

// authorizeAll requires want access on every non-empty object, returning the first denial. Handlers
// that reference several grantable objects at once, such as a run naming a project, an inventory,
// and credentials, use it to authorize each before acting.
func (a *authorizer) authorizeAll(ctx context.Context, want grant.Access, objects ...string) error {
	for _, obj := range objects {
		if obj == "" {
			continue
		}
		if err := a.authorize(ctx, obj, want); err != nil {
			return err
		}
	}
	return nil
}

// denyOnAuthzError writes the response for an authorization failure and reports whether the request
// was denied. A forbidden grant becomes 403; any other error becomes 500. A nil error is not a
// denial and returns false so the caller proceeds.
func denyOnAuthzError(w http.ResponseWriter, log *zap.Logger, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errForbiddenGrant) {
		forbidden(w)
		return true
	}
	log.Error("server: authorize: " + err.Error())
	respondError(w, log, http.StatusInternalServerError, "could not authorize request")
	return true
}

// authorizeRunAccess confirms the request actor may use the project, inventory, and credentials a
// run references, so a read or a run operation stays scoped to the objects the actor is granted when
// strict grants are on. It writes the denial and returns true when access is refused.
func authorizeRunAccess(w http.ResponseWriter, r *http.Request, authz *authorizer, log *zap.Logger, rn *run.Run) bool {
	objs := make([]string, 0, 2+len(rn.CredentialIDs))
	if rn.ProjectID != "" {
		objs = append(objs, rn.ProjectID)
	}
	if rn.InventoryID != "" {
		objs = append(objs, rn.InventoryID)
	}
	objs = append(objs, rn.CredentialIDs...)
	return denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objs...))
}
