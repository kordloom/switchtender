package server

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
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

// submitterOrg returns the organization a run submitted by actor is stamped with, so an objectless
// run is scoped to the actor's tenant. An actor in one organization stamps that one; an actor in
// several stamps the lexicographically smallest, a deterministic choice, since a run carries a
// single owning org and any org the actor belongs to isolates the run from every other tenant while
// leaving the actor able to read it. It returns empty when org membership is not wired, the actor is
// not a member of any organization, or the actor is a command-line token with no account.
func (a *authorizer) submitterOrg(ctx context.Context, actor Actor) (string, error) {
	if a == nil || a.orgs == nil || actor.UserID == "" {
		return "", nil
	}
	memberships, err := a.orgs.OrgsForUser(ctx, actor.UserID)
	if err != nil {
		return "", err
	}
	orgID := ""
	for _, m := range memberships {
		if orgID == "" || m.OrgID < orgID {
			orgID = m.OrgID
		}
	}
	return orgID, nil
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

// denyForeignOrg reports whether the actor may not place a template in orgID, and writes the denial
// when so. An empty orgID is unowned and always allowed.
//
// An organization is not a grantable object: resolveObjectOrg understands project, template,
// inventory and credential ids and nothing else, so passing an org id to authorizeAll resolves to
// not-found and, under strict grants, denies. Adding one to that list therefore refused every
// non-admin template write, which is the delegation the feature exists for. Membership is the right
// question, and subjectsFor already answers it.
func (a *authorizer) denyForeignOrg(w http.ResponseWriter, r *http.Request, log *zap.Logger,
	orgID string) bool {
	if a == nil || orgID == "" || !a.strict {
		return false
	}
	actor, ok := actorFrom(r.Context())
	if !ok || actor.Role == user.RoleAdmin {
		return false
	}
	subjects, err := a.subjectsFor(r.Context(), actor)
	if err != nil {
		respondError(w, log, http.StatusInternalServerError, "could not check organization access")
		return true
	}
	if subjects[orgID] {
		return false
	}
	respondError(w, log, http.StatusForbidden, "not a member of that organization")
	return true
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

// objectsFor returns the set of object ids the given subjects hold want on through a grant, so a list
// can be filtered to what the actor is allowed to see. The caller passes the actor's subjects so the org
// membership they carry is computed once and reused for the org-ownership check.
//
// The level is a parameter because a list of objects and a list of runs ask different questions: any
// grant satisfies read, which is right for seeing that a project exists, and wrong for reading the
// record of a change made through it.
func (a *authorizer) objectsFor(ctx context.Context, subjects map[string]bool,
	want grant.Access) (map[string]bool, error) {
	grants, err := a.grants.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for _, g := range grants {
		if subjects[g.Subject] && grant.Satisfies(g.Access, want) {
			out[g.Object] = true
		}
	}
	return out, nil
}

// runReadFilter is readFilter at use rather than read, for the views built out of runs.
//
// A run is not an object somebody was granted; it is a record of what was done to hosts through the
// objects it names. Fetching one by id asks for use on each of them, so a list of runs has to ask the
// same question or the list discloses what the fetch withholds.
func (a *authorizer) runReadFilter(ctx context.Context) (func(id, orgID string) bool, error) {
	return a.objectFilter(ctx, grant.AccessUse)
}

// readFilter returns a predicate reporting whether the request actor may see an object, given its id
// and owning organization, in a list. It keeps everything unless strict grants are on, since without
// strict mode reads defer to the global role and every role reads everything, and org ownership only
// ever adds access. Under strict grants a non-admin sees an object only when a grant lets them read it
// or they are a member of its owning organization, so another org's objects are excluded; an admin
// still sees all. A nil authorizer or grant store keeps everything. The error surfaces a grant-store
// failure so the caller can fail closed.
func (a *authorizer) readFilter(ctx context.Context) (func(id, orgID string) bool, error) {
	return a.objectFilter(ctx, grant.AccessRead)
}

// objectFilter is the shared body of readFilter and runReadFilter, deciding visibility at the given
// access level so the two cannot drift apart in anything but that level.
func (a *authorizer) objectFilter(ctx context.Context,
	want grant.Access) (func(id, orgID string) bool, error) {
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
	readable, err := a.objectsFor(ctx, subjects, want)
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
// derivedReadScan bounds how many recent runs are consulted when deciding what a derived view may
// show. The views themselves are already windowed, so this only has to cover the same ground.
const derivedReadScan = 2000

// derivedReadFilter returns a predicate deciding whether a row derived from a run may be shown, and
// whether the caller may see fleet-wide aggregates at all.
//
// Fleet health, drift, task trends, host history, host facts, and the worker list are all computed
// from runs. Every one of them returned the whole install to any viewer, including rows drawn from
// runs the same caller was refused a 403 on by name. The filter that already governs the run list
// governs these too; rows carrying a run id are checked against it, and an aggregate that names no
// run is shown only to a caller who can read something, because otherwise it is a summary of work
// they are not allowed to know about.
func derivedReadFilter(ctx context.Context, authz *authorizer,
	store run.Store) (keep func(runID string) bool, anyReadable bool, err error) {
	filter, err := authz.readFilter(ctx)
	if err != nil {
		return nil, false, err
	}
	// A keep-all filter means grants are not being enforced for this caller, so nothing changes and
	// the scan below is skipped entirely.
	if filter("proj_probe", "") && filter("cred_probe", "") {
		return func(string) bool { return true }, true, nil
	}
	page, err := store.ListPage(ctx, run.ListFilter{}, derivedReadScan, 0)
	if err != nil {
		return nil, false, err
	}
	// Decided the same way the run list decides it: a run is readable when every object it touches
	// is. Answering differently here would mean a host page and a run page disagreed about the same
	// run, which is its own kind of wrong.
	readable, err := readableRuns(ctx, authz, page)
	if err != nil {
		return nil, false, err
	}
	allowed := make(map[string]struct{}, len(readable))
	for _, rn := range readable {
		allowed[rn.ID] = struct{}{}
	}
	return func(id string) bool {
		_, ok := allowed[id]
		return ok
	}, len(allowed) > 0, nil
}

// grantsEnforced reports whether object grants actually restrict what this caller may read.
//
// It answers with the same probe readableRuns uses, so the two cannot disagree about whether a
// caller is filtered. A caller who is filtered must not be handed estate-wide aggregates: a fleet
// health table or a drift list is derived from every run on the install, including the ones their
// filter just removed, so passing one through would return exactly the rows the filter existed to
// withhold.
func grantsEnforced(ctx context.Context, authz *authorizer) (bool, error) {
	keep, err := authz.readFilter(ctx)
	if err != nil {
		return false, err
	}
	return !keep("proj_probe", "") || !keep("cred_probe", ""), nil
}

func readableRuns(ctx context.Context, authz *authorizer, runs []*run.Run) ([]*run.Run, error) {
	// Filtered at use, which is what fetching a run by id requires, rather than at read. Any grant
	// satisfies read, so filtering there put a run in the list whose by-id fetch answered 403: an
	// explicit read grant on one of its objects disclosed the whole run, its command, its extra vars
	// and the credentials it named, and a run's extra vars carry whatever a survey filled in. Reading a
	// run means reading what it did on hosts, so it takes the same access as using those objects.
	keep, err := authz.runReadFilter(ctx)
	if err != nil {
		return nil, err
	}
	// When the filter keeps everything, grants are not being enforced for this caller, so no object
	// needs its owning organization resolved and the list passes through untouched. Only a
	// strict-grants non-admin reaches the per-object resolution below.
	if keep("proj_probe", "") && keep("cred_probe", "") {
		return runs, nil
	}
	// A run is visible when every object it uses is, which is the rule authorize applies to fetch
	// one, so listing and fetching cannot disagree. An object owned by an organization the caller
	// belongs to is visible through that membership, so its owning org is resolved the same way
	// authorize resolves it and passed into the filter. Passing an empty org here dropped exactly
	// those runs: a strict-grants member saw none of their own org's runs in any run-derived view.
	orgOf := authz.orgResolverMemo(ctx)
	out := make([]*run.Run, 0, len(runs))
	for _, rn := range runs {
		objs := runObjects(rn)
		if len(objs) == 0 {
			// An objectless run has nothing for the per-object filter to decide on, so it is scoped
			// by the org it was stamped with. keep with an empty id resolves to that org's
			// membership alone: readable[""] is never set, so this is true only when the caller
			// belongs to the run's org, and an ownerless objectless run is dropped for every
			// strict-grants non-admin, matching what fetching it by id decides.
			if keep("", rn.OrgID) {
				out = append(out, rn)
			}
			continue
		}
		allowed := true
		for _, id := range objs {
			if !keep(id, orgOf(id)) {
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

// orgResolverMemo returns a function resolving an object's owning organization once and caching the
// result, so a run list referencing the same project or credential across many runs resolves each
// only once. It returns empty when org ownership is not wired or the object cannot be resolved, which
// leaves the read filter to decide on the grant alone.
func (a *authorizer) orgResolverMemo(ctx context.Context) func(object string) string {
	cache := make(map[string]string)
	return func(object string) string {
		if a == nil || a.orgOwners == nil {
			return ""
		}
		if orgID, ok := cache[object]; ok {
			return orgID
		}
		orgID, ok := a.orgOwners.OrgOf(ctx, object)
		if !ok {
			orgID = ""
		}
		cache[object] = orgID
		return orgID
	}
}

// runObjects lists the grantable objects a run uses.
//
// The registry credential that pulls the execution image is one of them. It was added to the run
// model and to the fields a retry inherits without widening this list, so a retry ran under a
// registry credential the actor was never granted, while the rerun handler happened to name it by
// hand and refused. Every path that asks what a run touches reads this list, so the two cannot
// disagree again.
func runObjects(rn *run.Run) []string {
	objs := make([]string, 0, 3+len(rn.CredentialIDs))
	if rn.ProjectID != "" {
		objs = append(objs, rn.ProjectID)
	}
	if rn.InventoryID != "" {
		objs = append(objs, rn.InventoryID)
	}
	if rn.PullCredentialID != "" {
		objs = append(objs, rn.PullCredentialID)
	}
	return append(objs, rn.CredentialIDs...)
}

// authorizeRun reports whether the request actor may exercise want on the run under strict grants.
//
// A run that references stored objects is scoped by them: the actor must be able to use every
// project, inventory, and credential it names, which is what authorizeAll checks and what org
// ownership of those objects already extends to their members. A run that references no stored
// object has nothing for that check to filter on, so it is scoped by the org it was stamped with at
// submit. Without this an objectless run, every inline script and every proposed run, was readable,
// cancelable, retryable, and approvable across every tenant, because authorizeAll over zero objects
// allows. Such a run is denied to a caller who is not in its org, and an objectless run with no
// owning org is denied to every non-admin under strict grants, the same as an ungranted object.
func (a *authorizer) authorizeRun(ctx context.Context, want grant.Access, rn *run.Run) error {
	objs := runObjects(rn)
	if len(objs) > 0 {
		return a.authorizeAll(ctx, want, objs...)
	}
	return a.authorizeOwningOrg(ctx, rn.OrgID)
}

// authorizeSchedule reports whether the request actor may exercise want on the schedule, the one
// question reading, editing, deleting, and listing one all ask, so they cannot disagree about who a
// schedule belongs to.
//
// A schedule that fires a stored template is scoped by that template, which org ownership of the
// template already extends to its members. A schedule that names no template carries its target
// inline, a playbook or a shell command line, and no grantable object at all, so it is scoped by the
// org it was stamped with when it was created. Without this an inline schedule was readable,
// rewritable, and deletable by any operator in any organization, because authorizeAll over zero
// objects allows: a crontab import lands hundreds of them, each holding the command line it runs.
// An inline schedule with no owning org is denied to every non-admin under strict grants, the same
// as an ungranted object.
func (a *authorizer) authorizeSchedule(ctx context.Context, want grant.Access,
	sc *schedule.Schedule) error {
	if sc.TemplateID != "" {
		return a.authorize(ctx, sc.TemplateID, want)
	}
	return a.authorizeOwningOrg(ctx, sc.OrgID)
}

// authorizeOwningOrg reports whether the request actor may act on an object owned by orgID when it
// carries nothing else to authorize against. It grants access to a member of that org and, under
// strict grants, denies everyone else; without strict grants it defers to the role like any
// ungranted object. A nil authorizer or grant store, an absent actor, or an admin all pass.
func (a *authorizer) authorizeOwningOrg(ctx context.Context, orgID string) error {
	if a == nil || a.grants == nil {
		return nil
	}
	actor, ok := actorFrom(ctx)
	if !ok || actor.Role == user.RoleAdmin {
		return nil
	}
	if !a.strict {
		return nil
	}
	if orgID != "" {
		subjects, err := a.subjectsFor(ctx, actor)
		if err != nil {
			return err
		}
		if subjects[orgID] {
			return nil
		}
	}
	return errForbiddenGrant
}

// authorizeRunAccess confirms the request actor may use the project, inventory, and credentials a
// run references, or belongs to the org an objectless run was stamped with, so a read or a run
// operation stays scoped when strict grants are on. It writes the denial and returns true when
// access is refused.
func authorizeRunAccess(w http.ResponseWriter, r *http.Request, authz *authorizer, log *zap.Logger, rn *run.Run) bool {
	return denyOnAuthzError(w, log, authz.authorizeRun(r.Context(), grant.AccessUse, rn))
}

// orgForUpdate resolves the owning organization an update should store: the one the request names,
// or the stored owner when the request names none at all.
//
// The field is a pointer for exactly this reason. Every edit dialog in the product sends the fields
// it renders and no others, and none of them renders an organization, so a rename arrived with the
// field absent, the handler wrote the zero value, and the record silently stopped belonging to its
// organization. Under strict grants its members lost it; otherwise every operator in the install
// gained it. Absent means keep, and a present empty string is the explicit "move this out".
func orgForUpdate(requested *string, stored string) string {
	if requested == nil {
		return stored
	}
	return *requested
}

// orgForCreate resolves the owning organization a create should store, treating an absent field as
// unowned.
func orgForCreate(requested *string) string {
	if requested == nil {
		return ""
	}
	return *requested
}

// intOrZero reads an optional integer, treating absent as zero.
func intOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
