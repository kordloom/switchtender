package scenario

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Invariant is a property every install must satisfy, checked after every scenario whatever that
// scenario was written for.
//
// This is the part that catches what unit tests do not. A defect that survives a package's own
// tests is rarely a wrong handler; it is handlers that are each right and together inconsistent. A
// list that shows what a fetch refuses. A derived view that names a run both of them hid. A
// response that carries a value the install was told to keep. None of those is visible from inside
// one handler, and all of them are visible here.
type Invariant struct {
	// Name is how a scenario names it in skip_invariants, and how a failure identifies itself.
	Name string
	// Why says what the property protects, so a failure is actionable without reading this file.
	Why string
	// Check reports every violation found, empty when the install satisfies the property. It is nil
	// for a property checked somewhere other than over the finished install, which the comment on
	// that entry names.
	Check func(in *Install) []string
}

// invariants is the battery, in the order it runs.
var invariants = []Invariant{{
	Name: "list_fetch_parity",
	Why: "A list and the by-id read behind it answer the same question, so they have to give the " +
		"same answer. A list that shows what the fetch refuses discloses the record it was " +
		"refusing; a list that hides what the fetch serves loses it. Both have shipped.",
	Check: checkListFetchParity,
}, {
	Name: "derived_views_agree",
	Why: "Fleet health, drift, host history and the change log are computed from runs, so a run " +
		"the list and the fetch both refuse must not be named by any of them. Filtering the list " +
		"and leaving these open returned exactly the rows the filter existed to withhold.",
	Check: checkDerivedViewsAgree,
}, {
	Name: "changes_agree",
	Why: "A change is what an auditor asks about and a run is what produced it, so a change built " +
		"entirely out of runs a caller may not read must not appear to them. Its rows carry no run " +
		"id, so the invariant that hunts ids cannot see this one.",
	Check: checkChangesAgree,
}, {
	Name: "no_secret_leak",
	Why: "A value the install was given to keep must not come back out of it. Run variables carry " +
		"whatever a survey filled in, and a list that masks one field and not another is a leak " +
		"with a tidy-looking response.",
	Check: checkNoSecretLeak,
}, {
	Name: "no_internal_errors",
	Why: "A 500 is never a correct answer to a well-formed read. It is the install telling an " +
		"operator that something broke without telling them what, which is the dead end this " +
		"product exists to stop producing.",
	Check: checkNoInternalErrors,
}, {
	Name: "refusals_explain_themselves",
	Why: "A refusal with an empty body is a dead end: the caller knows they were stopped and not " +
		"what to do about it. Every 4xx has to say what was wrong in words the person reading it " +
		"can act on.",
	Check: checkRefusalsExplainThemselves,
}, {
	Name: "writes_are_recorded",
	Why: "The audit chain is what the product sells. A write that changed the install and left no " +
		"entry is a gap in the record that no later verification can detect, because a chain with " +
		"an entry missing from the start verifies perfectly. It is checked after every write a " +
		"case makes rather than over the finished install, since only the write itself knows what " +
		"the chain held a moment before.",
	Check: nil,
}, {
	Name: "unauthenticated_reads_nothing",
	Why: "A caller with no credentials is not a caller with a role. Every read that answers one " +
		"has to refuse, or the whole authorization layer is reachable around rather than through.",
	Check: checkUnauthenticatedReadsNothing,
}}

// knownInvariant reports whether a name is one the battery holds, so a scenario cannot skip an
// invariant that does not exist and read as having opted out of something.
func knownInvariant(name string) bool {
	for _, inv := range invariants {
		if inv.Name == name {
			return true
		}
	}
	return false
}

// InvariantNames lists the battery in order.
func InvariantNames() []string {
	out := make([]string, 0, len(invariants))
	for _, inv := range invariants {
		out = append(out, inv.Name)
	}
	return out
}

// CheckInvariants runs the battery over a built install and returns every violation, naming the
// invariant and the reason it exists beside each one.
func CheckInvariants(in *Install) []string {
	var out []string
	for _, inv := range invariants {
		if inv.Check == nil {
			continue
		}
		if _, skipped := in.Scenario.SkipInvariants[inv.Name]; skipped {
			continue
		}
		for _, v := range inv.Check(in) {
			out = append(out, fmt.Sprintf("%s: %s\n    why this matters: %s", inv.Name, v, inv.Why))
		}
	}
	return out
}

// parityRoute is a listing and the by-id read behind its rows.
type parityRoute struct {
	// List is the listing path, with a limit high enough that pagination cannot hide a row and
	// make the parity check pass by accident.
	List string
	// ByID is the by-id path, with one verb for the id.
	ByID string
	// Kind names what is being listed, for the failure message.
	Kind string
	// Resource is the first path segment under /v1, which is how the guard matches this entry
	// against the routes the server mounts.
	Resource string
	// IDs returns the fixture ids of this kind, which is the population the two are compared over.
	IDs func(*Scenario) []string
}

// parityRoutes are the listings whose rows have a by-id read. A listing with none cannot disagree
// with anything, which is why most object listings are absent: projects, organizations and teams
// have a by-id write and no by-id read, so there is no second answer for a listing to contradict.
// The guard in the test file holds this table against the routes the server actually mounts, so a
// by-id read added later cannot quietly arrive without parity coverage.
var parityRoutes = []parityRoute{{
	List: "/v1/runs?limit=500", ByID: "/v1/runs/%s", Kind: "run", Resource: "runs",
	IDs: func(s *Scenario) []string {
		out := make([]string, 0, len(s.Fixtures.Runs))
		for _, r := range s.Fixtures.Runs {
			out = append(out, r.ID)
		}
		return out
	},
}, {
	List: "/v1/schedules", ByID: "/v1/schedules/%s", Kind: "schedule", Resource: "schedules",
	IDs: func(s *Scenario) []string {
		out := make([]string, 0, len(s.Fixtures.Schedules))
		for _, sc := range s.Fixtures.Schedules {
			out = append(out, sc.ID)
		}
		return out
	},
}}

// parityExclusions are resources with a by-id read that this suite does not yet build fixtures for.
// Each names what is missing rather than being silently absent, so the gap is a task and not a
// blind spot.
var parityExclusions = map[string]string{
	"credential-types": "no credential-type fixture yet",
	"changes":          "changes are derived from runs and are covered by derived_views_agree",
}

// derivedViews are the paths built out of runs that carry a run's id in their rows, so a view
// naming a refused run discloses it just as a list would and can be caught by looking for the id.
var derivedViews = []string{
	"/v1/fleet",
	"/v1/estate",
}

// derivedAggregates are built out of runs and carry no run id to look for, so the invariant that
// hunts ids cannot check them. What protects them is the withholding rule rather than per-row
// filtering, which is a different property and is checked differently.
//
// They are named rather than omitted. A path missing from both lists reads as a surface nobody
// thought about; a path here reads as one whose protection is stated.
var derivedAggregates = map[string]string{
	"/v1/tasks": "aggregated by task name over runs, with no run id in a row",
	"/v1/drift": "aggregated by host, with no run id in a row",
	"/v1/changes": "rows are changes, not runs; checked by checkChangesAgree below, which asks " +
		"whether a change whose every member run is refused still appears",
}

// checkListFetchParity holds every listing against the by-id read behind it, for every actor.
func checkListFetchParity(in *Install) []string {
	var out []string
	for _, actor := range in.Users {
		for _, route := range parityRoutes {
			ids := route.IDs(in.Scenario)
			if len(ids) == 0 {
				continue
			}
			list := in.Get(route.List, actor)
			if list.Status != http.StatusOK {
				// A listing this caller may not reach at all is consistent only if the by-id read
				// refuses every row too. A refused listing beside a served fetch is the same
				// disagreement in the other direction.
				for _, id := range ids {
					if fetch := in.Get(fmt.Sprintf(route.ByID, id), actor); fetch.Status == http.StatusOK {
						out = append(out, fmt.Sprintf(
							"%s listing %s answers %d for %s while %s fetches 200",
							route.Kind, route.List, list.Status, actor, id))
					}
				}
				continue
			}
			for _, id := range ids {
				fetch := in.Get(fmt.Sprintf(route.ByID, id), actor)
				served := fetch.Status == http.StatusOK
				listed := mentions(list.Body, id)
				switch {
				case served && !listed:
					out = append(out, fmt.Sprintf(
						"%s fetches 200 for %s and is absent from %s", id, actor, route.List))
				case !served && listed:
					out = append(out, fmt.Sprintf(
						"%s is in %s for %s and the by-id fetch answers %d",
						id, route.List, actor, fetch.Status))
				}
			}
		}
	}
	return out
}

// checkDerivedViewsAgree requires that no view built out of runs names a run its own by-id read
// refuses for the same caller.
func checkDerivedViewsAgree(in *Install) []string {
	var out []string
	runIDs := make([]string, 0, len(in.Scenario.Fixtures.Runs))
	for _, r := range in.Scenario.Fixtures.Runs {
		runIDs = append(runIDs, r.ID)
	}
	if len(runIDs) == 0 {
		return nil
	}
	paths := append([]string{}, derivedViews...)
	for _, r := range in.Scenario.Fixtures.Runs {
		if r.Host != "" {
			paths = append(paths, "/v1/hosts/"+r.Host+"/runs")
		}
	}
	sort.Strings(paths)
	paths = dedupe(paths)

	for _, actor := range in.Users {
		refused := map[string]int{}
		for _, id := range runIDs {
			if fetch := in.Get("/v1/runs/"+id, actor); fetch.Status != http.StatusOK {
				refused[id] = fetch.Status
			}
		}
		if len(refused) == 0 {
			continue
		}
		for _, path := range paths {
			view := in.Get(path, actor)
			if view.Status != http.StatusOK {
				continue
			}
			for id, status := range refused {
				if mentions(view.Body, id) {
					out = append(out, fmt.Sprintf(
						"%s names %s for %s, whose by-id fetch answers %d", path, id, actor, status))
				}
			}
		}
	}
	return out
}

// checkChangesAgree requires a change to be visible only to a caller who may read at least one run
// in it.
//
// The change log is the one run-derived view whose rows are not runs. It groups runs by a label, so
// the row carries a change name, a span, and an outcome derived from its members, and none of that
// is a run id. A caller refused every member of a change still learns that the change happened, when
// it ran, and whether it succeeded, which on a shared install is another group's release schedule.
func checkChangesAgree(in *Install) []string {
	members := map[string][]string{}
	for _, r := range in.Scenario.Fixtures.Runs {
		if r.Change != "" {
			members[r.Change] = append(members[r.Change], r.ID)
		}
	}
	if len(members) == 0 {
		return nil
	}
	var out []string
	for _, actor := range in.Users {
		body := in.Get("/v1/changes", actor).Body
		for change, runs := range members {
			readable := false
			for _, id := range runs {
				if in.Get("/v1/runs/"+id, actor).Status == http.StatusOK {
					readable = true
					break
				}
			}
			if !readable && mentions(body, change) {
				out = append(out, fmt.Sprintf(
					"/v1/changes names %s to %s, who is refused every run in it (%s)",
					change, actor, strings.Join(runs, ", ")))
			}
		}
	}
	return out
}

// checkNoSecretLeak hunts every declared secret through every read each actor can make.
//
// A value planted on a run is readable by whoever may read that run: that is the run's own content,
// not a leak. The leak is the value reaching an actor the run itself is refused to, which is
// exactly what a listing that discloses more than its fetch produces. A secret planted on nothing
// is entitled to nobody and must never appear anywhere.
func checkNoSecretLeak(in *Install) []string {
	secrets := append([]SecretFixture{}, in.Scenario.Fixtures.Secrets...)
	// A credential's sealed material is entitled to nobody, at any role, through any endpoint, so
	// every declared credential joins the hunt without the scenario having to say so twice.
	for _, c := range in.Scenario.Fixtures.Credentials {
		if c.Secret != "" {
			secrets = append(secrets, SecretFixture{Name: "credential " + c.ID, Value: c.Secret})
		}
	}
	secrets = append(secrets, in.Sealed...)
	if len(secrets) == 0 {
		return nil
	}
	var out []string
	paths := append([]string{
		"/v1/runs?limit=500", "/v1/projects", "/v1/audit", "/v1/credentials", "/v1/doctor",
		"/v1/templates", "/v1/schedules", "/v1/inventories", "/v1/triggers",
	}, derivedViews...)
	for _, r := range in.Scenario.Fixtures.Runs {
		paths = append(paths, "/v1/runs/"+r.ID)
		if r.Host != "" {
			paths = append(paths, "/v1/hosts/"+r.Host+"/runs")
		}
	}
	sort.Strings(paths)
	paths = dedupe(paths)

	for _, actor := range in.Users {
		entitled := map[string]bool{}
		for _, s := range secrets {
			entitled[s.Value] = holdsRole(in.Roles[actor], s.ToRoles) ||
				(s.OnRun != "" && in.Get("/v1/runs/"+s.OnRun, actor).Status == http.StatusOK)
		}
		for _, path := range paths {
			body := in.Get(path, actor).Body
			for _, s := range secrets {
				if s.Value == "" || entitled[s.Value] {
					continue
				}
				if strings.Contains(body, s.Value) {
					out = append(out, fmt.Sprintf("%s returns the %s value to %s%s",
						path, s.Name, actor, entitlement(s)))
				}
			}
		}
	}
	return out
}

// checkUnauthenticatedReadsNothing requires every read to refuse a caller with no credentials.
//
// It walks the same surface the other status invariants do, rather than the two listings it used
// to. Those two were the only paths it asked about, and for a scenario whose fixtures held neither
// runs nor schedules it asked about nothing at all while still reporting a pass, so making a
// listing public would not have failed it. The by-id reads matter as much as the listings.
func checkUnauthenticatedReadsNothing(in *Install) []string {
	var out []string
	for _, path := range probePaths(in.Scenario) {
		res := in.Get(path, "")
		if res.Status == http.StatusOK {
			out = append(out, fmt.Sprintf("%s answered 200 with no credentials: %s",
				path, firstLine(res.Body)))
		}
	}
	return out
}

// holdsRole reports whether an actor's global role is among those a value is entitled to.
func holdsRole(role string, entitled []string) bool {
	for _, want := range entitled {
		if role == want {
			return true
		}
	}
	return false
}

// entitlement explains, in a failure message, why this value should not have reached this caller.
func entitlement(s SecretFixture) string {
	switch {
	case len(s.ToRoles) > 0:
		return ", who is not " + strings.Join(s.ToRoles, " or ") +
			", the only role this value is shown to"
	case s.OnRun != "":
		return ", who is refused " + s.OnRun + ", the run it was planted on"
	default:
		return ", and this value is entitled to nobody: no role, no grant, and no endpoint returns it"
	}
}

// CheckModeNarrowing holds the two grant modes against each other.
//
// What --strict-grants decides is narrow and documented: it is the default for an object nobody has
// granted, and nothing else. Everything it changes must therefore be a removal. If turning it on
// ever shows an actor something the open install did not, the two modes have drifted into two
// authorization systems that happen to share a flag, and the documented sentence describing the
// difference is no longer true of the code.
//
// The comparison is free. The scenario has already been built twice.
func CheckModeNarrowing(open, strict *Install) []string {
	var out []string
	ids := declaredIDs(open.Scenario)
	if len(ids) == 0 {
		return nil
	}
	for _, actor := range open.Users {
		for _, path := range probePaths(open.Scenario) {
			strictBody := strict.Get(path, actor).Body
			openBody := open.Get(path, actor).Body
			for _, id := range ids {
				if mentions(strictBody, id) && !mentions(openBody, id) {
					out = append(out, fmt.Sprintf(
						"strict_only_narrows: %s shows %s to %s under strict grants and not on the "+
							"open install, so turning strict grants on widened what this caller "+
							"sees. The flag decides the default for an ungranted object and "+
							"nothing else, so every difference it makes has to be a removal.",
						path, id, actor))
				}
			}
		}
	}
	return out
}

// declaredIDs are the fixture ids a body can name, which is the population the two modes are
// compared over.
func declaredIDs(s *Scenario) []string {
	var out []string
	for _, r := range s.Fixtures.Runs {
		out = append(out, r.ID)
	}
	for _, p := range s.Fixtures.Projects {
		out = append(out, p.ID)
	}
	for _, sc := range s.Fixtures.Schedules {
		out = append(out, sc.ID)
	}
	for _, tp := range s.Fixtures.Templates {
		out = append(out, tp.ID)
	}
	for _, c := range s.Fixtures.Credentials {
		out = append(out, c.ID)
	}
	return out
}

// probePaths are every read the battery exercises, which is the surface the status invariants below
// hold to answering something an operator can act on.
func probePaths(s *Scenario) []string {
	paths := append([]string{
		"/v1/runs?limit=500", "/v1/projects", "/v1/credentials", "/v1/templates", "/v1/schedules",
		"/v1/inventories", "/v1/triggers", "/v1/orgs", "/v1/teams", "/v1/users", "/v1/grants",
		"/v1/audit", "/v1/doctor", "/v1/workers",
	}, derivedViews...)
	for _, r := range s.Fixtures.Runs {
		paths = append(paths, "/v1/runs/"+r.ID)
		if r.Host != "" {
			paths = append(paths, "/v1/hosts/"+r.Host+"/runs", "/v1/hosts/"+r.Host+"/facts")
		}
	}
	for _, sc := range s.Fixtures.Schedules {
		paths = append(paths, "/v1/schedules/"+sc.ID)
	}
	// Ids nobody minted, because a read that only ever meets real rows has never been asked what it
	// does with a miss, and a miss answered with a 500 is the same dead end as a crash.
	paths = append(paths, "/v1/runs/run_no_such_thing", "/v1/schedules/sched_no_such_thing")
	sort.Strings(paths)
	return dedupe(paths)
}

// checkNoInternalErrors requires every read to answer something other than a server error, for
// every actor and for a caller with no credentials at all.
func checkNoInternalErrors(in *Install) []string {
	var out []string
	actors := append([]string{""}, in.Users...)
	for _, actor := range actors {
		for _, path := range probePaths(in.Scenario) {
			res := in.Get(path, actor)
			if res.Status >= 500 {
				out = append(out, fmt.Sprintf("%s answered %d for %s (%s)",
					path, res.Status, actorName(actor), firstLine(res.Body)))
			}
		}
	}
	return out
}

// checkRefusalsExplainThemselves requires every refusal to carry a reason.
func checkRefusalsExplainThemselves(in *Install) []string {
	var out []string
	actors := append([]string{""}, in.Users...)
	for _, actor := range actors {
		for _, path := range probePaths(in.Scenario) {
			res := in.Get(path, actor)
			if res.Status < 400 || res.Status >= 500 {
				continue
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := res.JSON(&body); err != nil || strings.TrimSpace(body.Error) == "" {
				out = append(out, fmt.Sprintf(
					"%s refused %s with %d and no reason a reader can act on (%s)",
					path, actorName(actor), res.Status, firstLine(res.Body)))
			}
		}
	}
	return out
}

// actorName labels a caller in a failure message, including the one with no credentials.
func actorName(actor string) string {
	if actor == "" {
		return "a caller with no credentials"
	}
	return actor
}

// identityKeys are the JSON members that carry a row's identity. A body names a row when the id
// appears under one of these, and not merely when those bytes occur somewhere in it.
var identityKeys = map[string]bool{
	"id": true, "run_id": true, "change": true, "template_id": true, "schedule_id": true,
}

// referenceKeys are members whose string elements are references to rows rather than content. A
// fleet row carries its recent runs as a bare array of ids, so the id sits under no member of its
// own and only the array it is in says what it is.
var referenceKeys = map[string]bool{
	"recent_runs": true, "run_ids": true, "runs": true, "members": true,
}

// contentKeys are members that carry values somebody typed, which are content rather than
// references. A run id appearing inside one is a string a person wrote, not a row in this list.
var contentKeys = map[string]bool{
	"extra_vars": true, "settings": true, "facts": true, "env": true, "vars": true,
	"command": true, "payload": true, "steps": true,
}

// mentions reports whether a body names the given row.
//
// It decodes and looks for the id under an identity member, rather than scanning the bytes. The
// substring form it replaced could not tell a listed row from a value that happens to contain the
// id, and a run's extra variables are free text somebody typed: one run whose variables name
// another run's id made the parity invariant read that row as present when it had been dropped from
// the list, hiding the exact disagreement the invariant exists to catch, and read it as present in a
// tenant's list that correctly did not contain it, inventing one.
//
// A body that is not JSON falls back to the quoted-substring test, because a non-JSON response
// carrying the id is a disclosure whatever its shape.
func mentions(body, id string) bool {
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return strings.Contains(body, `"`+id+`"`)
	}
	return namesRow(decoded, id, false)
}

// namesRow walks decoded JSON for the id under a member that references a row, skipping the members
// that carry what somebody typed. keyed says whether the value being looked at sits under such a
// member, which is what an array of bare ids needs: the id there has no member of its own, and only
// the array it is in says what it is.
func namesRow(v any, id string, keyed bool) bool {
	switch t := v.(type) {
	case string:
		return keyed && t == id
	case map[string]any:
		for key, val := range t {
			if contentKeys[key] {
				continue
			}
			if namesRow(val, id, identityKeys[key] || referenceKeys[key]) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if namesRow(val, id, keyed) {
				return true
			}
		}
	}
	return false
}

// firstLine is a body trimmed to something a failure message can carry.
func firstLine(body string) string {
	body = strings.TrimSpace(body)
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	}
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return body
}

// dedupe removes adjacent duplicates from a sorted slice.
func dedupe(in []string) []string {
	out := in[:0]
	var prev string
	for i, v := range in {
		if i == 0 || v != prev {
			out = append(out, v)
		}
		prev = v
	}
	return out
}
