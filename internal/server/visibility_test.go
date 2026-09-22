package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheFilterAnswersWhatTheFetchAnswers is the guard the run list needed and did not have.
//
// A listing cannot call authorize per row, so it decides rows from a reduction of the grant table
// built once per request. That reduction is a second implementation of the access rule, and it has
// disagreed with the first in both directions. It disclosed: the list was unfiltered on an open
// install and returned runs whose by-id fetch answered 403, with their command, their extra vars and
// the credentials they named. Then it concealed: the fix filtered on "the caller holds a grant",
// which is a different question, so an install with an empty grant table showed every non-admin an
// empty run list while every one of those runs still fetched 200.
//
// Neither direction is catchable by testing one side. This walks every object shape the fixture has
// against every caller, in both grant modes, and requires the two answers to be the same answer.
func TestTheFilterAnswersWhatTheFetchAnswers(t *testing.T) {
	t.Parallel()
	newAuthz := orgOwnedFixture(t)

	// Every shape a grant table can put an object in: owned and granted to an outsider, owned and
	// ungranted, owned by somebody else's organization, unowned but granted, and unowned and
	// ungranted. The last is the default install's only shape and the one the regression hid.
	objects := []string{"proj_a", "proj_solo", "proj_b", "proj_granted", "proj_unknown"}
	// Non-admin actors only. An admin bypasses both paths before either consults a grant, so it
	// proves nothing about whether they agree.
	actors := []string{"user_admin_a", "user_member_a", "user_b", "user_outsider"}
	// The levels the two filters are built at. Manage is not among them because no listing asks for
	// it: object visibility is decided at read and a run at use.
	levels := []grant.Access{grant.AccessRead, grant.AccessUse}

	for _, strict := range []bool{false, true} {
		for _, want := range levels {
			t.Run(fmt.Sprintf("strict=%v/%s", strict, want), func(t *testing.T) {
				t.Parallel()
				authz := newAuthz(strict)
				for _, uid := range actors {
					ctx := ctxActor(uid, user.RoleOperator)
					// filterInEveryMode, because this compares against a by-id answer, and the by-id
					// answer does not depend on the grant mode for an object carrying grants.
					vis, err := authz.visibilityFor(ctx, want, filterInEveryMode, everyObject)
					if err != nil {
						t.Fatalf("visibilityFor(%s) error = %v", want, err)
					}
					orgOf := authz.orgResolverMemo(ctx)
					for _, obj := range objects {
						listed := vis.allows(obj, orgOf(obj))
						fetched := authz.authorize(ctx, obj, want) == nil
						if listed != fetched {
							t.Errorf("%s on %s: the list says visible=%v and the by-id fetch says "+
								"allowed=%v. A list that shows what the fetch refuses discloses it; "+
								"a list that hides what the fetch serves loses it.",
								uid, obj, listed, fetched)
						}
					}
				}
			})
		}
	}
}

// TestRestrictedReaderReportsWhetherGrantsCanHideAnything pins the question the install-wide
// aggregates are gated on.
//
// Fleet health, task trends, the run-count summary and the metrics exposition are computed from
// every run on the install and carry no per-row id to filter, so they are served whole or not at
// all. That decision used to be made by asking the filter about a made-up object id, the same
// expression copied into five places, and it asked whether the caller holds a grant rather than
// whether any grant can hide something from them. On an open install it called every caller
// unrestricted, which is how the run list came to refuse a run while the fleet view named it.
func TestRestrictedReaderReportsWhetherGrantsCanHideAnything(t *testing.T) {
	t.Parallel()
	ctx := ctxActor("user_nobody", user.RoleOperator)

	tests := []struct {
		Name   string
		Authz  *authorizer
		Want   bool
		Reason string
	}{{ // Test 0: The default install, nothing delegated. Nothing can be hidden, so nothing is.
		Name:   "open install with an empty grant table",
		Authz:  &authorizer{grants: &fakeGrants{byObject: map[string][]*grant.Grant{}}},
		Want:   false,
		Reason: "no grant exists, so no object is access controlled and this caller loses nothing",
	}, { // Test 1: One delegation to somebody else. Now something can be hidden.
		Name: "open install with one object delegated to another subject",
		Authz: &authorizer{grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"proj_delegated": {{Subject: "user_contractor", Access: grant.AccessUse}},
		}}},
		Want:   true,
		Reason: "proj_delegated is access controlled and this caller does not hold it",
	}, { // Test 2: The caller holds the only grant, so the table hides nothing from them.
		Name: "open install where the caller holds every granted object",
		Authz: &authorizer{grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"proj_theirs": {{Subject: "user_nobody", Access: grant.AccessUse}},
		}}},
		Want:   false,
		Reason: "the only controlled object is held by this caller",
	}, { // Test 3: Strict grants deny every ungranted object, which restricts by itself.
		Name:   "strict grants with an empty grant table",
		Authz:  &authorizer{strict: true, grants: &fakeGrants{byObject: map[string][]*grant.Grant{}}},
		Want:   true,
		Reason: "under strict grants an ungranted object denies, so the whole install is hidden",
	}, { // Test 4: No grant store wired at all. Object access is not being decided.
		Name:   "no grant store",
		Authz:  &authorizer{},
		Want:   false,
		Reason: "object level access is not wired, so nothing restricts anyone",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := restrictedReader(ctx, test.Authz)
			if err != nil {
				t.Fatalf("restrictedReader() error = %v", err)
			}
			if got != test.Want {
				t.Errorf("%s: restricted = %v, want %v: %s", test.Name, got, test.Want, test.Reason)
			}
		})
	}
}

// TestTheOpenInstallServesTheRunsItAllows is the end-to-end half, over the wiring cmd serve ships.
//
// A grant store is always wired and strict grants default to off, so this is the shape of every
// install that has not opted into strict mode. One project is delegated to a contractor and the
// caller is an unrelated operator: the delegated project's run is theirs to lose, and the other two
// are not. The run list, the by-id fetch and the host history all have to say the same thing about
// each of the three, because a run the fetch refuses must not surface in a list and a run the fetch
// serves must not vanish from one.
func TestTheOpenInstallServesTheRunsItAllows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	users := user.NewMemStore()
	orgs := org.NewMemStore()
	grants := grant.NewMemStore()
	projects := project.NewMemStore()
	runs := run.NewMemStore()

	for _, id := range []string{"proj_ops", "proj_delegated"} {
		if err := projects.Save(ctx, &project.Project{
			ID: id, Name: id, RepoURL: "https://example.com/" + id + ".git",
		}); err != nil {
			t.Fatalf("Save() project %s error = %v", id, err)
		}
	}

	const host = "web1"
	base := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	rows := []struct {
		ID      string
		Project string
	}{
		{ID: "run_a", Project: "proj_ops"},
		{ID: "run_b", Project: "proj_delegated"},
		{ID: "run_c", Project: "proj_ops"},
	}
	for i, row := range rows {
		rn := &run.Run{
			ID: row.ID, Playbook: "site.yml", ProjectID: row.Project, Status: run.StatusRunning,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := runs.Save(ctx, rn); err != nil {
			t.Fatalf("Save() run %s error = %v", row.ID, err)
		}
		// Recorded while the run is still going, because the store fences a terminal run's summary
		// against a reclaimed worker overwriting it.
		if err := runs.SaveHostSummary(ctx, row.ID, []run.HostSummary{{Host: host, OK: 1}}); err != nil {
			t.Fatalf("SaveHostSummary(%s) error = %v", row.ID, err)
		}
		rn.Status = run.StatusSucceeded
		if err := runs.Save(ctx, rn); err != nil {
			t.Fatalf("Save() finished run %s error = %v", row.ID, err)
		}
	}

	// The contractor the one project was delegated to, and the operator who was not.
	contractor, err := user.New("contractor", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, contractor); err != nil {
		t.Fatalf("Save() user error = %v", err)
	}
	operator, err := user.New("operator", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, operator); err != nil {
		t.Fatalf("Save() user error = %v", err)
	}
	if err := grants.Save(ctx, &grant.Grant{
		ID: "grant_delegated", Subject: contractor.ID, Object: "proj_delegated",
		Access: grant.AccessUse, CreatedAt: base,
	}); err != nil {
		t.Fatalf("Save() grant error = %v", err)
	}

	// The default wiring: a grant store, strict grants off.
	handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithGrants(grants, false), WithOrgs(orgs), WithProjects(projects), WithUsers(users)).Handler()

	as := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), actorKey{},
			Actor{UserID: operator.ID, Role: user.RoleOperator, Name: "operator"}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// What the by-id fetch answers for each run is the standard every view is held to.
	allowed := map[string]bool{}
	for _, row := range rows {
		rec := as("/v1/runs/" + row.ID)
		switch rec.Code {
		case http.StatusOK:
			allowed[row.ID] = true
		case http.StatusForbidden:
			allowed[row.ID] = false
		default:
			t.Fatalf("fetching %s = %d, want 200 or 403 (body %s)", row.ID, rec.Code, rec.Body.String())
		}
	}
	if !allowed["run_a"] || !allowed["run_c"] {
		t.Fatalf("the fetch refused a run on a project nobody has granted, on an install that has "+
			"not turned strict grants on: %v", allowed)
	}
	if allowed["run_b"] {
		t.Fatalf("the fetch served a run on a project delegated to somebody else: %v", allowed)
	}

	// The list.
	list := as("/v1/runs")
	if list.Code != http.StatusOK {
		t.Fatalf("listing runs = %d (body %s)", list.Code, list.Body.String())
	}
	var listed struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
		Summary struct {
			Scope string `json:"scope"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode run list: %v", err)
	}
	inList := map[string]bool{}
	for _, r := range listed.Runs {
		inList[r.ID] = true
	}
	for id, ok := range allowed {
		if inList[id] != ok {
			t.Errorf("run %s: the by-id fetch says allowed=%v and the list says present=%v. On the "+
				"default install every non-admin was handed an empty run list while every one of "+
				"those runs still fetched 200.", id, ok, inList[id])
		}
	}
	// A caller whose rows are filtered must not be told the install's totals, since that number is
	// every other tenant's volume.
	if listed.Summary.Scope == "install" {
		t.Error("the summary reports install-wide totals to a caller whose run list was filtered")
	}

	// The host page is built out of the same runs and must not name the one the fetch refuses.
	history := as("/v1/hosts/" + host + "/runs")
	if history.Code != http.StatusOK {
		t.Fatalf("host history = %d (body %s)", history.Code, history.Body.String())
	}
	var hist struct {
		Runs []struct {
			RunID string `json:"run_id"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &hist); err != nil {
		t.Fatalf("decode host history: %v", err)
	}
	onHost := map[string]bool{}
	for _, h := range hist.Runs {
		onHost[h.RunID] = true
	}
	for id, ok := range allowed {
		if onHost[id] != ok {
			t.Errorf("run %s: the by-id fetch says allowed=%v and the host page says present=%v. "+
				"The run list and the fetch both refused this run while the host, fleet and drift "+
				"views went on naming it, its host and its outcome.", id, ok, onHost[id])
		}
	}
}

// TestAMalformedGrantDoesNotHideEverything covers a row the API will not write and a database can
// still hold.
//
// A run with no objects is decided under the empty id, so a grant whose object is empty would mark
// that id access controlled and hide every objectless run from every non-admin at once. Nothing
// would report it: the runs simply stop appearing, install-wide, on the strength of one malformed
// row that an import or a hand-edited table can leave behind. The reduction skips it instead.
func TestAMalformedGrantDoesNotHideEverything(t *testing.T) {
	t.Parallel()
	authz := &authorizer{grants: &fakeGrants{byObject: map[string][]*grant.Grant{
		"": {{Subject: "somebody_else", Access: grant.AccessUse}},
	}}}
	ctx := ctxActor("user_ops", user.RoleOperator)

	vis, err := authz.visibilityFor(ctx, grant.AccessUse, filterInEveryMode, runScopedObjects)
	if err != nil {
		t.Fatalf("visibilityFor() error = %v", err)
	}
	if !vis.allows("", "") {
		t.Error("a grant with no object hid every objectless run from a non-admin on an open " +
			"install, which is every run that names no project, inventory or credential")
	}
	if vis.restricted() {
		t.Error("a grant with no object reported this caller as restricted, which withholds every " +
			"install-wide total from them")
	}
}

// TestADelegationOfSomethingNoRunNamesRestrictsNobody covers a blackout with no visible cause.
//
// A grant on a template or a worker queue is an ordinary delegation, and neither can hide a run:
// a run names a project, an inventory and credentials, and nothing else. Counting one when deciding
// whether grants restrict a caller reported every caller who was not that grant's subject as
// restricted, on an install where nothing was hidden from them at all.
//
// What that costs is not a shorter list. The run list still returns every row. The totals beside it
// silently change from the install's to the caller's own, the task table answers withheld, and the
// Prometheus exposition answers 200 with no bytes at all: every series disappears rather than
// changing value, so an alert on a series going to zero never fires again. Nothing logs, and nothing
// connects any of it to the template grant that caused it.
func TestADelegationOfSomethingNoRunNamesRestrictsNobody(t *testing.T) {
	t.Parallel()
	ctx := ctxActor("user_unrelated", user.RoleOperator)

	for _, object := range []string{"tpl_deploy", grant.QueueObject("prod")} {
		t.Run(object, func(t *testing.T) {
			t.Parallel()
			authz := &authorizer{grants: &fakeGrants{byObject: map[string][]*grant.Grant{
				object: {{Subject: "team_sre", Access: grant.AccessUse}},
			}}}
			restricted, err := restrictedReader(ctx, authz)
			if err != nil {
				t.Fatalf("restrictedReader() error = %v", err)
			}
			if restricted {
				t.Errorf("a grant on %s reported this caller as restricted, so every install-wide "+
					"total is withheld from them and the metrics exposition answers with no series, "+
					"on an install where no run is hidden from them at all", object)
			}
		})
	}

	// A grant on something a run does name still restricts, or the narrowing has gone too far and
	// the disclosure this whole reduction exists to prevent comes back.
	authz := &authorizer{grants: &fakeGrants{byObject: map[string][]*grant.Grant{
		"proj_delegated": {{Subject: "user_contractor", Access: grant.AccessUse}},
	}}}
	restricted, err := restrictedReader(ctx, authz)
	if err != nil {
		t.Fatalf("restrictedReader() error = %v", err)
	}
	if !restricted {
		t.Error("a grant on a project this caller does not hold reported them as unrestricted, so " +
			"they would be served install-wide totals covering runs they cannot read")
	}
}
