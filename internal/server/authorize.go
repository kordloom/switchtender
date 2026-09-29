package server

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/user"
)

// errForbiddenGrant is returned by authorize when the actor lacks a grant the object requires.
var errForbiddenGrant = errors.New("forbidden: no grant for this object")

// OrgResolver resolves the owning organization of a grantable object by its id, so the authorizer can
// extend access to that organization's members. A resolver reads the object's stored org id from
// whichever store owns the object kind.
type OrgResolver interface {
	// OrgOf returns the owning organization id of the object with the given id, and whether the object
	// was found. An empty org id with ok true means the object exists but is unowned; ok false means
	// the object could not be resolved and the caller treats it as unowned.
	OrgOf(ctx context.Context, objectID string) (orgID string, ok bool)
}

// OrgResolverFunc adapts a function to an OrgResolver.
type OrgResolverFunc func(ctx context.Context, objectID string) (string, bool)

// OrgOf calls f.
func (f OrgResolverFunc) OrgOf(ctx context.Context, objectID string) (string, bool) {
	return f(ctx, objectID)
}

// authorizer decides object-level access on top of the coarse global role. It is additive: an
// object with no grants defers to the role, unless strict grants are on, which flips an object with
// no grants to deny for non-admins. Owning-organization membership adds access on top of grants and,
// under strict grants, isolates an org-owned object to its members and anyone explicitly granted.
type authorizer struct {
	// grants stores per-object access grants; nil disables object-level checks entirely.
	grants grant.Store
	// teams resolves an actor's team memberships so team grants apply to their members.
	teams team.Store
	// orgs resolves an actor's organization memberships so org grants apply to their members.
	orgs org.Store
	// orgOwners resolves a grantable object's owning organization so its members gain access to it.
	// Nil disables org-ownership access, leaving only grants and the role in force.
	orgOwners OrgResolver
	// strict makes an object with no grants deny non-admins instead of deferring to the role.
	strict bool
}

// authorize reports whether the request's actor may exercise want access on object. Admins and
// command-line tokens bypass. The ordered checks are: an explicit grant to the actor, one of their
// teams, or one of their organizations comes first; then membership in the object's owning
// organization, which an org admin exercises as manage and a plain member as use. Both are additive:
// they only ever grant. When neither grants access, an object carrying grants denies an unmatched
// actor, and an ungranted object defers to the role unless strict grants deny it. Under strict grants
// an org-owned object is denied to a non-member, which is the tenant isolation. It returns
// errForbiddenGrant when access is denied.
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
	if len(grants) > 0 {
		subjects, err := a.subjectsFor(ctx, actor)
		if err != nil {
			return err
		}
		for _, g := range grants {
			if subjects[g.Subject] && grant.Satisfies(g.Access, want) {
				return nil
			}
		}
	}

	// Owning-organization membership adds access on top of grants: a member gains their org role's
	// access, which never denies what a grant or the role already allows.
	have, member, err := a.orgAccess(ctx, actor, object)
	if err != nil {
		return err
	}
	if member && grant.Satisfies(have, want) {
		return nil
	}

	// Nothing granted access. An object carrying grants is access-controlled, so an unmatched actor is
	// denied. An ungranted object defers to the role, unless strict grants deny it: under strict grants
	// an org-owned object seen here belongs to an org the actor is not a member of, so isolation denies
	// it, and an unowned object is denied for want of a grant, the unchanged strict behavior.
	if len(grants) > 0 {
		return errForbiddenGrant
	}
	if a.strict {
		return errForbiddenGrant
	}
	return nil
}

// orgAccess reports the access level object's owning organization confers on actor, and whether the
// actor is a member of that organization. An org admin manages the org's objects; a plain member may
// use them. It returns ok false when org ownership is not wired, the object has no owning org, the
// owner cannot be resolved, or the actor is not a member, so it only ever adds access and never takes
// it away.
func (a *authorizer) orgAccess(ctx context.Context, actor Actor, object string) (grant.Access, bool, error) {
	if a.orgOwners == nil || a.orgs == nil || actor.UserID == "" {
		return "", false, nil
	}
	orgID, ok := a.orgOwners.OrgOf(ctx, object)
	if !ok || orgID == "" {
		return "", false, nil
	}
	memberships, err := a.orgs.OrgsForUser(ctx, actor.UserID)
	if err != nil {
		return "", false, err
	}
	for _, m := range memberships {
		if m.OrgID != orgID {
			continue
		}
		if m.Role == org.RoleAdmin {
			return grant.AccessManage, true, nil
		}
		return grant.AccessUse, true, nil
	}
	return "", false, nil
}

// subjectsFor returns the set of grant subject ids that represent the actor: their own user id and
// every team they belong to.
func (a *authorizer) subjectsFor(ctx context.Context, actor Actor) (map[string]bool, error) {
	subjects := map[string]bool{}
	if actor.UserID == "" {
		return subjects, nil
	}
	subjects[actor.UserID] = true
	if a.teams != nil {
		teamIDs, err := a.teams.TeamsForUser(ctx, actor.UserID)
		if err != nil {
			return nil, err
		}
		for _, tid := range teamIDs {
			subjects[tid] = true
		}
	}
	if a.orgs != nil {
		memberships, err := a.orgs.OrgsForUser(ctx, actor.UserID)
		if err != nil {
			return nil, err
		}
		for _, m := range memberships {
			subjects[m.OrgID] = true
		}
	}
	return subjects, nil
}

// manages reports whether actor holds management authority over object through an explicit manage
// grant, admin of the object's owning organization, or the global admin role. Unlike authorize it
// never defers to the global role for an ungranted object, since management delegation requires an
// explicit grant or org ownership, so the caller falls back to the role gate when this returns false.
// A nil authorizer or grant store confers no management.
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
	// An admin of the object's owning organization manages it, the same delegation an explicit manage
	// grant confers. This only adds management, since the caller falls back to the role gate on false.
	have, member, err := a.orgAccess(ctx, actor, object)
	if err != nil {
		return false, err
	}
	if member && grant.Satisfies(have, grant.AccessManage) {
		return true, nil
	}
	return false, nil
}

// readableObjectsFor returns the set of object ids the given subjects may read through a grant, so a
// list can be filtered to what the actor is allowed to see. Any grant satisfies read, since use and
// manage rank above it. The caller passes the actor's subjects so the org membership they carry is
// computed once and reused for the org-ownership check.
func (a *authorizer) readableObjectsFor(ctx context.Context, subjects map[string]bool) (map[string]bool, error) {
	grants, err := a.grants.List(ctx)
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

// readFilter returns a predicate reporting whether the request actor may see an object, given its id
// and owning organization, in a list. It keeps everything unless strict grants are on, since without
// strict mode reads defer to the global role and every role reads everything, and org ownership only
// ever adds access. Under strict grants a non-admin sees an object only when a grant lets them read it
// or they are a member of its owning organization, so another org's objects are excluded; an admin
// still sees all. A nil authorizer or grant store keeps everything. The error surfaces a grant-store
// failure so the caller can fail closed.
func (a *authorizer) readFilter(ctx context.Context) (func(id, orgID string) bool, error) {
	keepAll := func(_, _ string) bool { return true }
	if a == nil || a.grants == nil || !a.strict {
		return keepAll, nil
	}
	actor, ok := actorFrom(ctx)
	if !ok || actor.Role == user.RoleAdmin {
		return keepAll, nil
	}
	subjects, err := a.subjectsFor(ctx, actor)
	if err != nil {
		return nil, err
	}
	readable, err := a.readableObjectsFor(ctx, subjects)
	if err != nil {
		return nil, err
	}
	// The subjects set carries every organization the actor belongs to, so subjects[orgID] reports
	// membership in the object's owning org. A read (or higher) grant makes an object visible; so does
	// membership in the org that owns it.
	return func(id, orgID string) bool {
		return readable[id] || (orgID != "" && subjects[orgID])
	}, nil
}

// filterReadable returns the items the request actor may see, dropping any object a strict-grants
// deployment has neither granted the actor read on nor placed in an organization the actor belongs to.
// id extracts an item's object id and orgOf its owning organization id. It errors only when the grant
// store fails, so a list handler fails closed rather than leaking everything.
func filterReadable[T any](ctx context.Context, authz *authorizer, items []T, id, orgOf func(T) string) ([]T, error) {
	keep, err := authz.readFilter(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		if keep(id(it), orgOf(it)) {
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

// readableRuns drops any run the caller may not read. A run is readable when every object it uses is,
// which is the same rule fetching one run applies, so listing and fetching cannot disagree.
func readableRuns(ctx context.Context, authz *authorizer, runs []*run.Run) ([]*run.Run, error) {
	keep, err := authz.readFilter(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*run.Run, 0, len(runs))
	for _, rn := range runs {
		allowed := true
		for _, id := range runObjects(rn) {
			if !keep(id, "") {
				allowed = false
				break
			}
		}
		if allowed {
			out = append(out, rn)
		}
	}
	return out, nil
}

// runObjects lists the grantable objects a run uses.
func runObjects(rn *run.Run) []string {
	objs := make([]string, 0, 2+len(rn.CredentialIDs))
	if rn.ProjectID != "" {
		objs = append(objs, rn.ProjectID)
	}
	if rn.InventoryID != "" {
		objs = append(objs, rn.InventoryID)
	}
	return append(objs, rn.CredentialIDs...)
}

// authorizeRunAccess confirms the request actor may use the project, inventory, and credentials a
// run references, so a read or a run operation stays scoped to the objects the actor is granted when
// strict grants are on. It writes the denial and returns true when access is refused.
func authorizeRunAccess(w http.ResponseWriter, r *http.Request, authz *authorizer, log *zap.Logger, rn *run.Run) bool {
	return denyOnAuthzError(w, log,
		authz.authorizeAll(r.Context(), grant.AccessUse, runObjects(rn)...))
}
