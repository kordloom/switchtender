package server

import (
	"context"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/user"
)

// grantRule decides object access from the three facts a grant table states about one object: whether
// the caller holds the access asked for, whether the object carries any grant at all, and whether
// strict grants are on.
//
// It is the whole of the object-level rule, and it has exactly one implementation on purpose. Two
// paths ask it: authorize, which resolves the three facts for a single object out of that object's
// grants, and visibility, which resolves them for every object at once out of the whole grant list.
// Those paths must agree, because a list that answers differently from the by-id fetch either
// discloses what the fetch withholds or hides what it serves. Both have happened. The list was
// unfiltered on an open install and returned runs the fetch answered 403 on; the fix filtered it on
// "the caller holds a grant", which is not this rule, and an install with no grants at all then
// showed every non-admin an empty run list while every one of those runs still fetched 200.
//
// Held wins because grants and org membership only ever add access. A controlled object denies an
// unmatched caller, since writing a grant on an object is what declares it access-controlled. An
// object nobody has granted defers to the global role, which is the open default, unless strict
// grants are on, where an ungranted object denies instead.
func grantRule(held, controlled, strict bool) bool {
	switch {
	case held:
		return true
	case controlled:
		return false
	default:
		return !strict
	}
}

// visibility is one caller's view of the grant table at one access level, resolved once per request.
//
// A listing cannot ask authorize per row. The filter it needs is assembled from every grant on the
// install, and rebuilding it per row made one fleet read cost rows times grants. So the grant list is
// reduced once, here, into the facts grantRule needs, and every row is decided from that reduction.
type visibility struct {
	// subjects is every subject the caller acts as: their user id, their teams, their organizations.
	// An object owned by an organization in this set is visible through that membership.
	subjects map[string]bool
	// held is the set of object ids a grant gives the caller the asked-for access on.
	held map[string]bool
	// controlled is the set of object ids carrying any grant, at any level and to any subject. An
	// object in this set is access-controlled, so a caller who does not hold it is denied in both
	// grant modes.
	controlled map[string]bool
	// strict reports whether strict grants are on, which denies an object carrying no grants.
	strict bool
	// all reports that every question answers yes without consulting the sets: the caller is an
	// admin, no grant store is wired, or this level is not filtered on an open install.
	all bool
}

// allows reports whether the caller may see the object with this id and owning organization. It is
// the list-shaped answer to what authorize answers for one object, and it answers with the same rule.
func (v visibility) allows(id, orgID string) bool {
	if v.all {
		return true
	}
	held := v.held[id] || (orgID != "" && v.subjects[orgID])
	return grantRule(held, v.controlled[id], v.strict)
}

// restricted reports whether grants can hide anything on this install from this caller, which is what
// decides whether an install-wide aggregate may be served whole. A fleet health table or a run-count
// total is derived from every run on the install, so handing one to a caller whose rows are filtered
// returns exactly what the filter exists to withhold.
//
// It reads the grant table rather than probing the filter with a made-up object id. That probe was
// the same expression in five places and it asked the wrong question: it reported whether the caller
// holds a grant, not whether any grant can hide something from them, so on an open install it called
// every caller unrestricted and the run-derived views went out unfiltered.
//
// Held is a subset of controlled, since an object is only held through a grant written on it, so a
// count comparison answers whether some controlled object is unheld.
//
// What is counted matters as much as the comparison. The reduction this is asked of is scoped to
// the objects a run can name, because a grant on a template or a worker queue hides no run from
// anybody: counting one reported every caller who was not its subject as restricted, on an install
// where nothing was hidden from them, and the run list still returned every row while the totals
// beside it went blank and the metrics exposition answered with no series at all.
//
// One over-report remains and is deliberate. An object the caller reaches through membership in its
// owning organization rather than through a grant counts as unheld, because resolving every
// controlled object's owner would cost a store read per object on every request that asks. That
// direction withholds an aggregate from someone entitled to it; the other publishes another
// tenant's volume.
func (v visibility) restricted() bool {
	if v.all {
		return false
	}
	return v.strict || len(v.controlled) > len(v.held)
}

// objectScope narrows a reduction to the object kinds the asking view can actually meet. A view
// that decides runs must not count a grant on a template or a queue, since no run names one and a
// grant on one therefore hides nothing from anybody.
type objectScope func(object string) bool

// everyObject admits every grantable object, for a listing that decides object rows itself.
func everyObject(string) bool { return true }

// runScopedObjects admits only the objects a run names, which is what the run-derived views decide
// rows with. The set lives in the grant package beside the object kinds, held to run.RunAuth by a
// guard there, so the two cannot drift apart silently.
func runScopedObjects(object string) bool { return grant.ScopesARun(object) }

// visibilityFor resolves the caller's view of the grant table at the given access level. whenOpen
// says whether the level is filtered on an install that has not turned strict grants on, and scope
// which object kinds the asking view can meet.
//
// The level is a parameter because a list of objects and a list of runs ask different questions: any
// grant satisfies read, which is right for seeing that a project exists and wrong for reading the
// record of a change made through it.
func (a *authorizer) visibilityFor(ctx context.Context, want grant.Access, whenOpen bool,
	scope objectScope) (visibility, error) {
	if a == nil || a.grants == nil {
		return visibility{all: true}, nil
	}
	if !a.strict && !whenOpen {
		return visibility{all: true}, nil
	}
	actor, ok := actorFrom(ctx)
	if !ok || actor.Role == user.RoleAdmin {
		return visibility{all: true}, nil
	}
	subjects, err := a.subjectsFor(ctx, actor)
	if err != nil {
		return visibility{}, err
	}
	held, controlled, err := a.objectsFor(ctx, subjects, want, scope)
	if err != nil {
		return visibility{}, err
	}
	return visibility{subjects: subjects, held: held, controlled: controlled, strict: a.strict}, nil
}

// restrictedReader reports whether object grants restrict what this caller may see, resolved at the
// access level reading a run takes. Every view that serves an install-wide aggregate asks it, so they
// cannot disagree about which callers are filtered.
func restrictedReader(ctx context.Context, authz *authorizer) (bool, error) {
	vis, err := authz.visibilityFor(ctx, grant.AccessUse, filterInEveryMode, runScopedObjects)
	if err != nil {
		return false, err
	}
	return vis.restricted(), nil
}
