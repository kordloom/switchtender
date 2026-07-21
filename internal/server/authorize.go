package server

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/grant"
	"github.com/dcadolph/switchtender/internal/run"
	"github.com/dcadolph/switchtender/internal/team"
	"github.com/dcadolph/switchtender/internal/user"
)

// errForbiddenGrant is returned by authorize when the actor lacks a grant the object requires.
var errForbiddenGrant = errors.New("forbidden: no grant for this object")

// authorizer decides object-level access on top of the coarse global role. It is additive: an
// object with no grants defers to the role, unless strict grants are on, which flips an object with
// no grants to deny for non-admins.
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

// manages reports whether actor holds management authority over object through an explicit manage
// grant, or is an admin. Unlike authorize it never defers to the global role for an ungranted
// object, since management delegation requires an explicit grant, so the caller falls back to the
// role gate when this returns false. A nil authorizer or grant store confers no management.
func (a *authorizer) manages(ctx context.Context, actor Actor, object string) (bool, error) {
	if a == nil || a.grants == nil {
		return false, nil
	}
	if actor.Role == user.RoleAdmin {
		return true, nil
	}
	grants, err := a.grants.ForObject(ctx, object)
	if err != nil {
		return false, err
	}
	subjects, err := a.subjectsFor(ctx, actor)
	if err != nil {
		return false, err
	}
	for _, g := range grants {
		if subjects[g.Subject] && grant.Satisfies(g.Access, grant.AccessManage) {
			return true, nil
		}
	}
	return false, nil
}

// readableObjects returns the set of object ids the actor may read through a grant, so a list can be
// filtered to what the actor is allowed to see. Any grant satisfies read, since use and manage rank
// above it.
func (a *authorizer) readableObjects(ctx context.Context, actor Actor) (map[string]bool, error) {
	grants, err := a.grants.List(ctx)
	if err != nil {
		return nil, err
	}
	subjects, err := a.subjectsFor(ctx, actor)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for _, g := range grants {
		if subjects[g.Subject] && grant.Satisfies(g.Access, grant.AccessRead) {
			out[g.Object] = true
		}
	}
	return out, nil
}

// readFilter returns a predicate reporting whether the request actor may see an object id in a list.
// It keeps everything unless strict grants are on, since without strict mode reads defer to the global
// role and every role reads everything. Under strict grants a non-admin sees only objects a grant lets
// them read, while an admin still sees all. A nil authorizer or grant store keeps everything. The error
// surfaces a grant-store failure so the caller can fail closed.
func (a *authorizer) readFilter(ctx context.Context) (func(id string) bool, error) {
	keepAll := func(string) bool { return true }
	if a == nil || a.grants == nil || !a.strict {
		return keepAll, nil
	}
	actor, ok := actorFrom(ctx)
	if !ok || actor.Role == user.RoleAdmin {
		return keepAll, nil
	}
	readable, err := a.readableObjects(ctx, actor)
	if err != nil {
		return nil, err
	}
	return func(id string) bool { return readable[id] }, nil
}

// filterReadable returns the items the request actor may see, dropping any object a strict-grants
// deployment has not granted the actor read on. id extracts an item's object id. It errors only when
// the grant store fails, so a list handler fails closed rather than leaking everything.
func filterReadable[T any](ctx context.Context, authz *authorizer, items []T, id func(T) string) ([]T, error) {
	keep, err := authz.readFilter(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		if keep(id(it)) {
			out = append(out, it)
		}
	}
	return out, nil
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
