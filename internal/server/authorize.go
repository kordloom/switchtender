package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/user"
)

// errForbiddenGrant is returned by authorize when the actor lacks a grant the object requires.
var errForbiddenGrant = errors.New("forbidden: no grant for this object")

// errForbiddenOrg is returned when an object carrying nothing grantable is denied because the actor
// does not belong to the organization that owns it.
//
// It is a separate sentinel from errForbiddenGrant because the two have different remedies and the
// refusal names one. An inline schedule and a run naming no project are scoped by their owning
// organization alone, and grant.ValidObject refuses a grant written on either of them, so telling
// that operator to obtain a grant sends them to an API that answers "object must be a proj_, tpl_,
// inv_, or cred_ id". A refusal that names an impossible remedy is worse than one that names none.
var errForbiddenOrg = errors.New("forbidden: not a member of the organization that owns this")

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
	held := false
	if len(grants) > 0 {
		subjects, serr := a.subjectsFor(ctx, actor)
		if serr != nil {
			return serr
		}
		for _, g := range grants {
			if subjects[g.Subject] && grant.Satisfies(g.Access, want) {
				held = true
				break
			}
		}
	}

	// Owning-organization membership adds access on top of grants: a member gains their org role's
	// access, which never denies what a grant or the role already allows. An explicit grant already
	// decided the answer, so it short circuits the owner lookup, which costs a store read.
	if !held {
		have, member, oerr := a.orgAccess(ctx, actor, object)
		if oerr != nil {
			return oerr
		}
		held = member && grant.Satisfies(have, want)
	}

	// grantRule finishes it, and finishes the same question for every listing on the install. An
	// object carrying grants is access-controlled, so an unmatched actor is denied; an ungranted
	// object defers to the role unless strict grants deny it. Under strict grants an org-owned object
	// seen here belongs to an org the actor is not a member of, so isolation denies it, and an unowned
	// object is denied for want of a grant, the unchanged strict behavior.
	if grantRule(held, len(grants) > 0, a.strict) {
		return nil
	}
	return errForbiddenGrant
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
		// Organization admin confers manage over that organization's objects, but never above what
		// the account's own global role allows.
		//
		// The ceiling is the point. Without it, an operator who created a read-only auditor and
		// added it to an organization as "admin", meaning "let them see all of this", handed that
		// account write on the organization's credentials, templates, projects and inventories. The
		// name invites exactly that mistake, and nothing in the product corrected it.
		//
		// The global role is the account's ceiling everywhere else, including for agents one file
		// away in authmw: an agent gets its role and nothing more. Membership is delegation within
		// what an account may already do, not a promotion past it. An organization still administers
		// itself without install-wide admin, because its admins are operators.
		//
		// An explicit per-object manage grant is deliberately left alone: that is an admin choosing
		// to delegate one named object to one named subject, which is a decision somebody made,
		// rather than a role name meaning more than it says.
		if m.Role == org.RoleAdmin && roleAllows(actor.Role, user.RoleOperator) {
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

// objectsFor reduces the whole grant list to the two facts a listing decides rows with: the objects
// the given subjects hold want on, and the objects carrying any grant at all. The caller passes the
// actor's subjects so the org membership they carry is computed once and reused.
//
// Both sets come from one pass because both are needed for every row and they answer different
// halves of the rule: held says the caller may see it, controlled says the object is access
// controlled and so denies everyone else. Returning only the first is what made a list disagree with
// the fetch, since an object nobody has granted is absent from it for a reason that has nothing to
// do with the caller.
func (a *authorizer) objectsFor(ctx context.Context, subjects map[string]bool, want grant.Access,
	scope objectScope) (held, controlled map[string]bool, err error) {
	grants, err := a.grants.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	held, controlled = make(map[string]bool), make(map[string]bool)
	for _, g := range grants {
		// A grant naming no object is not a grant on everything, and it must not become one here.
		// The API refuses to write one, but an import, a migration, or a hand-edited row can leave
		// one behind, and the empty id is the one a run with no objects is decided under: marking
		// it access controlled would hide every objectless run from every non-admin, silently and
		// install-wide, on the strength of a single malformed row.
		if g.Object == "" {
			continue
		}
		// An object kind the asking view can never meet is not a restriction on it.
		if !scope(g.Object) {
			continue
		}
		controlled[g.Object] = true
		if subjects[g.Subject] && grant.Satisfies(g.Access, want) {
			held[g.Object] = true
		}
	}
	return held, controlled, nil
}

// Whether a list is filtered on an install that has not turned strict grants on. A list with a
// matching by-id read must be, or it discloses what that read refuses; a list with none need not be,
// and the documented rule for those is that a read grant scopes them under strict grants.
const (
	filterInEveryMode     = true
	filterUnderStrictOnly = false
)

// readFilter returns a predicate reporting whether the request actor may see an object, given its id
// and owning organization, in a list of objects. Under strict grants a non-admin sees an object only
// when a grant lets them read it or they are a member of its owning organization, so another org's
// objects are excluded; an admin still sees all. A nil authorizer or grant store keeps everything.
// The error surfaces a grant-store failure so the caller can fail closed.
//
// It keeps everything on an open install, and that is deliberate rather than an oversight. None of
// the objects it filters has a by-id read, so there is no second answer for a listing to disagree
// with, and the documented rule for them is that a read grant scopes a listing under strict grants.
// Scoping them always would hide objects an operator granted in order to delegate, not in order to
// conceal.
//
// The views built out of runs do the opposite, and ask at use rather than read: a run is not an
// object somebody was granted, it is a record of what was done to hosts through the objects it
// names, so fetching one asks for use on each of them and a listing has to ask the same question or
// it discloses what the fetch withholds. Those resolve a visibility directly, since they need to
// know whether the caller is restricted at all as well as which rows they may see.
func (a *authorizer) readFilter(ctx context.Context) (func(id, orgID string) bool, error) {
	vis, err := a.visibilityFor(ctx, grant.AccessRead, filterUnderStrictOnly, everyObject)
	if err != nil {
		return nil, err
	}
	return vis.allows, nil
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

// queueObject names a worker queue as a grant object, or returns empty when no queue was asked for
// so authorizeAll skips it the way it skips any unset object.
//
// The queues a worker token may lease from were confined by the pool file and described in the code
// as the blast radius of that token. The queue a submitter could ask for was checked nowhere: it was
// copied off the request body onto the run, no authorizer or policy referenced the field, and the
// claim query filters on queue alone. So a low-privilege caller could post queue "prod" and have the
// production relay execute their run inside the production segment, on a host holding that segment's
// machine identity. Authorizing it here puts the submit side under the same rules as the lease side.
func queueObject(queue string) string {
	queue = strings.TrimSpace(queue)
	if queue == "" {
		return ""
	}
	return grant.QueueObject(queue)
}

// allowQueue refuses a named queue on an install that cannot run a worker to serve it.
//
// A queue restricts a run to workers serving that name, and every worker is Team. On Community
// nothing can ever claim such a run: the field saved cleanly, the run was accepted, and it sat
// pending forever with no error anywhere to explain it. The product sells queues as Team and did
// not gate them, so the failure mode was a silently stranded run rather than a refusal naming the
// tier, which is the opposite of how every other gate here behaves.
//
// The default queue is always allowed: that is the server's own pool, which needs no worker.
func allowQueue(queue string) error {
	if strings.TrimSpace(queue) == "" {
		return nil
	}
	return license.Allow(license.FeatureWorkers)
}

// denyOnAuthzError writes the response for an authorization failure and reports whether the request
// was denied. A forbidden grant becomes 403; any other error becomes 500. A nil error is not a
// denial and returns false so the caller proceeds.
func denyOnAuthzError(w http.ResponseWriter, log *zap.Logger, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errForbiddenGrant) {
		forbiddenGrant(w)
		return true
	}
	if errors.Is(err, errForbiddenOrg) {
		forbiddenOrg(w)
		return true
	}
	log.Error("server: authorize: " + err.Error())
	respondError(w, log, http.StatusInternalServerError, "could not authorize request")
	return true
}

// forbiddenGrant refuses a request the object-level rules denied, saying that the grants stopped it
// rather than the role.
//
// The generic refusal is one word, which is the right answer to a caller who has no business here
// at all and the wrong one to an operator with a valid account. An install that turns on strict
// grants denies every object nobody has granted yet, which is documented and correct and, answered
// with "forbidden", is indistinguishable from a broken account, a wrong role, or an install with
// nothing in it. The operator has no way to tell which, and the one that is true is the only one
// they can fix.
//
// It says nothing the caller does not already know. It does not report whether the object exists,
// whether anyone else holds a grant on it, or which of the object-level rules denied it: a
// nonexistent object and a delegated one refuse identically, with this same sentence. What it adds
// is the one fact that turns a stop into a next step, which is that a grant is what is missing.
func forbiddenGrant(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"forbidden: this object requires a grant you do not hold"}`))
}

// forbiddenOrg refuses a request denied by organization ownership, naming membership as what is
// missing rather than a grant that cannot be written.
func forbiddenOrg(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(
		`{"error":"forbidden: this belongs to an organization you are not a member of"}`))
}

// derivedReadScan bounds how many recent runs are consulted when deciding what a derived view may
// show. The views themselves are already windowed, so this only has to cover the same ground. A
// test that needs a smaller window passes one to derivedReadFilterIn rather than bending this: it
// was a var once, and a test shrinking it raced every parallel test on the derived-read path.
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
	return derivedReadFilterIn(ctx, authz, store, derivedReadScan)
}

// derivedReadFilterIn is derivedReadFilter with the scan window passed in, so the aged-out-of-window
// case is testable without touching shared state.
func derivedReadFilterIn(ctx context.Context, authz *authorizer, store run.Store,
	scan int) (keep func(runID string) bool, anyReadable bool, err error) {
	// The view is built out of runs, so it is decided at the access level reading a run takes, and
	// the same visibility answers both questions below. Asking the object-read filter instead left
	// these views unfiltered on every install that had not turned strict grants on: the run list and
	// the by-id fetch both refused a run while fleet health, drift, and host history went on naming
	// it, its host, and its outcome.
	//
	// It is resolved once for the whole request and shared by every row. It is assembled from the
	// entire grant table, so rebuilding it per row made one fleet or drift read cost rows times
	// grants: at a thousand rows and ten thousand grants a single request took seconds and allocated
	// a gigabyte. It does not depend on which run is being decided, so hoisting it changes no answer.
	// A grant-store failure is reported here instead of quietly hiding one row, which still refuses
	// rather than discloses.
	vis, err := authz.visibilityFor(ctx, grant.AccessUse, filterInEveryMode, runScopedObjects)
	if err != nil {
		return nil, false, err
	}
	// Grants restrict nothing for this caller, so every row is theirs to see and the scan below is
	// skipped entirely.
	if !vis.restricted() {
		return func(string) bool { return true }, true, nil
	}
	runKeep := vis.allows
	orgOf := authz.orgResolverMemo(ctx)
	// Whether the caller can read anything decides only whether estate-wide aggregates that name no
	// run are shown at all, so a bounded probe of recent runs answers it. Which individual rows show
	// is decided per run below, not from this scan.
	anyReadable, err = probeAnyReadable(ctx, store, runKeep, orgOf, scan)
	if err != nil {
		return nil, false, err
	}
	// A row is shown when its own governing run is readable, checked on demand and memoized, however
	// old that run is. The predicate used to keep only the ids of the 2000 newest runs, so on a busy
	// install a grant-restricted caller silently lost every drift, host-history, and host-facts row
	// whose run had aged past that window: the fix has to consult the actual governing run, not a
	// recent slice of the whole install. A run is readable when every object it touches is, the same
	// rule the run list applies, so a host page and a run page cannot disagree about one run.
	seen := make(map[string]bool)
	keep = func(id string) bool {
		if v, ok := seen[id]; ok {
			return v
		}
		// The retained decision, not the run. Derived rows outlive the runs that produced them on
		// purpose, and retention deletes runs, so resolving the run made every summary, drift row,
		// and state reading whose run had aged out unreadable to a grant-restricted caller. Not
		// refused and not explained: simply absent, at exactly the depth an audit asks about.
		//
		// Still one rule and still failing closed. A run with no retained decision either never
		// existed or predates the retaining, and both read as unreadable.
		ok := false
		if auth, gerr := store.RunAuthFor(ctx, id); gerr == nil {
			ok = runReadable(auth, runKeep, orgOf)
		}
		seen[id] = ok
		return ok
	}
	return keep, anyReadable, nil
}

// derivedReadProbeHead is how many of the newest runs the aggregate probe looks at before it falls
// back to the full derivedReadScan window.
//
// A caller who reads anything at all almost always reads something recent, and one readable run is
// the whole answer, so materializing two thousand rows on every fleet, drift, host, worker, and
// metrics request to find it was work the answer never used. The head is a prefix of the same
// ordering the full window walks, so a run found here would have been found there: the answer is
// unchanged, only the common case stops earlier.
const derivedReadProbeHead = 100

// probeAnyReadable reports whether the caller can read any recent run, which is what decides if
// the estate-wide aggregates that name no run are shown at all. It checks the newest runs first
// and only widens to the full derivedReadScan window when that head holds nothing readable, so the
// answer matches the full scan while the usual request pays for a fraction of it.
func probeAnyReadable(ctx context.Context, store run.Store, keep func(id, orgID string) bool,
	orgOf func(string) string, scan int) (bool, error) {
	head := min(derivedReadProbeHead, scan)
	for _, limit := range [2]int{head, scan} {
		page, err := store.ListPage(ctx, run.ListFilter{}, limit, 0)
		if err != nil {
			return false, err
		}
		for _, rn := range page {
			if runReadable(run.AuthOf(rn), keep, orgOf) {
				return true, nil
			}
		}
		// A short head means the install holds fewer runs than the head asked for, so the wider window
		// would read exactly the same rows again and reach the same answer.
		if limit == scan || len(page) < limit {
			break
		}
	}
	return false, nil
}

// readableRuns drops any run the caller may not read. A run is readable when every object it uses is,
// which is the same rule fetching one run applies, so listing and fetching cannot disagree.
func readableRuns(ctx context.Context, authz *authorizer, runs []*run.Run) ([]*run.Run, error) {
	// Decided at use, which is what fetching a run by id requires, rather than at read. Any grant
	// satisfies read, so filtering there put a run in the list whose by-id fetch answered 403: an
	// explicit read grant on one of its objects disclosed the whole run, its command, its extra vars
	// and the credentials it named, and a run's extra vars carry whatever a survey filled in. Reading a
	// run means reading what it did on hosts, so it takes the same access as using those objects.
	//
	// The visibility is resolved once for the whole list. It is assembled from every grant on the
	// install, so resolving it per run made a run list cost the whole grant table once per row.
	vis, err := authz.visibilityFor(ctx, grant.AccessUse, filterInEveryMode, runScopedObjects)
	if err != nil {
		return nil, err
	}
	// Grants restrict nothing for this caller, so no object needs its owning organization resolved
	// and the list passes through untouched.
	if !vis.restricted() {
		return runs, nil
	}
	orgOf := authz.orgResolverMemo(ctx)
	out := make([]*run.Run, 0, len(runs))
	for _, rn := range runs {
		if runReadable(run.AuthOf(rn), vis.allows, orgOf) {
			out = append(out, rn)
		}
	}
	return out, nil
}

// runReadable reports whether one run passes the read filter.
//
// A run is visible when every object it uses is, which is the rule authorize applies to fetch one,
// so listing and fetching cannot disagree. An object owned by an organization the caller belongs
// to is visible through that membership, so its owning org is resolved the same way authorize
// resolves it and passed into the filter. Passing an empty org here dropped exactly those runs: a
// strict-grants member saw none of their own org's runs in any run-derived view.
func runReadable(auth *run.RunAuth, keep func(id, orgID string) bool, orgOf func(string) string) bool {
	if auth == nil {
		return false
	}
	objs := auth.Objects()
	if len(objs) == 0 {
		// An objectless run has nothing for the per-object filter to decide on, so it is scoped by the
		// org it was stamped with. keep with an empty id resolves to that org's membership alone:
		// readable[""] is never set, so this is true only when the caller belongs to the run's org, and
		// an ownerless objectless run is dropped for every strict-grants non-admin, matching what
		// fetching it by id decides.
		return keep("", auth.OrgID)
	}
	for _, id := range objs {
		if !keep(id, orgOf(id)) {
			return false
		}
	}
	return true
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
// The list itself lives on run.RunAuth, which is also what a purged run leaves behind, so a live
// run and a retained decision are scoped by the same objects computed by the same code. Keeping a
// second copy here would be the same disagreement this comment already records, one layer down.
func runObjects(rn *run.Run) []string {
	return run.AuthOf(rn).Objects()
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
	return errForbiddenOrg
}

// authorizeRunAccess confirms the request actor may use the project, inventory, and credentials a
// run references, or belongs to the org an objectless run was stamped with, so a read or a run
// operation stays scoped when strict grants are on. It writes the denial and returns true when
// access is refused.
func authorizeRunAccess(w http.ResponseWriter, r *http.Request, authz *authorizer, log *zap.Logger, rn *run.Run) bool {
	return denyOnAuthzError(w, log, authz.authorizeRun(r.Context(), grant.AccessUse, rn))
}

// authorizeReexecute authorizes re-running rn's spec: the objects reading it needs, and then the
// worker queue, which only a re-execution reaches.
//
// The direct launch authorizes the queue with AccessUse alongside the project, inventory and
// credentials before it submits. Retry and relaunch re-run the same spec against the same queue and
// authorized everything except the queue, so an operator who could touch a run but held no grant on
// its queue executed on that queue by retrying. The queue is not folded into authorizeRunAccess
// because it is not part of a run's readability: reading a run's record is not running work on the
// network its queue names, and reads must not start requiring a queue grant.
func authorizeReexecute(w http.ResponseWriter, r *http.Request, authz *authorizer,
	log *zap.Logger, rn *run.Run) bool {
	if authorizeRunAccess(w, r, authz, log, rn) {
		return true
	}
	if rn.Queue == "" {
		return false
	}
	return denyOnAuthzError(w, log,
		authz.authorizeAll(r.Context(), grant.AccessUse, grant.QueueObject(rn.Queue)))
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
