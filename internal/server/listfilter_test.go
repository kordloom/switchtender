package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// countingGrants counts the grant reads a listing makes, so a test can hold it to one table read.
type countingGrants struct {
	grant.Store
	// lists is how many times List was called.
	lists atomic.Int64
	// perObject is how many times ForObject was called.
	perObject atomic.Int64
}

// List counts the call and delegates.
func (c *countingGrants) List(ctx context.Context) ([]*grant.Grant, error) {
	c.lists.Add(1)
	return c.Store.List(ctx)
}

// ForObject counts the call and delegates.
func (c *countingGrants) ForObject(ctx context.Context, object string) ([]*grant.Grant, error) {
	c.perObject.Add(1)
	return c.Store.ForObject(ctx, object)
}

// listFilterFixture is the grant, team, and organization state the list-equivalence tests decide
// rows against, with the template owners a resolver reads.
type listFilterFixture struct {
	// grants is the grant store, wrapped to count reads.
	grants *countingGrants
	// teams holds the one team the direct holder belongs to.
	teams team.Store
	// orgs holds org_a and org_b and their members.
	orgs org.Store
	// owners maps each template id to its owning organization.
	owners map[string]string
	// resolves counts single-object owner lookups.
	resolves atomic.Int64
	// ownerReads counts whole-table owner reads.
	ownerReads atomic.Int64
}

// Users and objects the matrix is built from. The direct holder holds grants itself and through a
// team, the member reaches templates through its organization, the org admin is an admin of that
// organization, and the outsider holds nothing.
const (
	lfHolder   = "user_holder"
	lfMember   = "user_member"
	lfOrgAdmin = "user_orgadmin"
	lfOutsider = "user_outsider"
	lfOther    = "user_other"
	lfTeam     = "team_holders"
)

// lfTemplates are the templates rows reference, one per way a grant can decide a row.
var lfTemplates = []string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
	"tpl_direct",        // A use grant to the holder.
	"tpl_team",          // A use grant to the holder's team.
	"tpl_readonly",      // A read grant to the holder, which use does not accept.
	"tpl_foreign",       // A use grant to somebody else, so controlled.
	"tpl_org_a",         // Owned by org_a, no grants.
	"tpl_org_a_granted", // Owned by org_a and granted to somebody else.
	"tpl_org_b",         // Owned by org_b, no grants.
	"tpl_free",          // Neither owned nor granted.
	"tpl_org_grant",     // A use grant to org_a as a subject.
	"tpl_missing_owner", // Granted to somebody else and unknown to the resolver.
}

// newListFilterFixture builds the grant, team, and organization state the matrix runs over.
func newListFilterFixture(t *testing.T) *listFilterFixture {
	t.Helper()
	ctx := context.Background()
	f := &listFilterFixture{
		grants: &countingGrants{Store: grant.NewMemStore()},
		teams:  team.NewMemStore(),
		orgs:   org.NewMemStore(),
		owners: map[string]string{
			"tpl_org_a": "org_a", "tpl_org_a_granted": "org_a", "tpl_org_b": "org_b",
			"tpl_direct": "", "tpl_team": "", "tpl_readonly": "", "tpl_foreign": "", "tpl_free": "",
			"tpl_org_grant": "",
		},
	}
	grants := []*grant.Grant{
		{ID: "g1", Subject: lfHolder, Object: "tpl_direct", Access: grant.AccessUse},
		{ID: "g2", Subject: lfTeam, Object: "tpl_team", Access: grant.AccessUse},
		{ID: "g3", Subject: lfHolder, Object: "tpl_readonly", Access: grant.AccessRead},
		{ID: "g4", Subject: lfOther, Object: "tpl_foreign", Access: grant.AccessUse},
		{ID: "g5", Subject: lfOther, Object: "tpl_org_a_granted", Access: grant.AccessUse},
		{ID: "g6", Subject: "org_a", Object: "tpl_org_grant", Access: grant.AccessUse},
		{ID: "g7", Subject: lfOther, Object: "tpl_missing_owner", Access: grant.AccessUse},
		{ID: "g8", Subject: lfOther, Object: "proj_unrelated", Access: grant.AccessUse},
	}
	for _, g := range grants {
		if err := f.grants.Save(ctx, g); err != nil {
			t.Fatalf("grants.Save(%s) error = %v", g.ID, err)
		}
	}
	if err := f.teams.Save(ctx, &team.Team{ID: lfTeam, Name: "holders"}); err != nil {
		t.Fatalf("teams.Save() error = %v", err)
	}
	if err := f.teams.AddMember(ctx, lfTeam, lfHolder); err != nil {
		t.Fatalf("teams.AddMember() error = %v", err)
	}
	for _, id := range []string{"org_a", "org_b"} {
		if err := f.orgs.Save(ctx, &org.Org{ID: id, Name: id}); err != nil {
			t.Fatalf("orgs.Save(%s) error = %v", id, err)
		}
	}
	if err := f.orgs.AddMember(ctx, "org_a", lfMember, org.RoleMember); err != nil {
		t.Fatalf("orgs.AddMember(member) error = %v", err)
	}
	if err := f.orgs.AddMember(ctx, "org_a", lfOrgAdmin, org.RoleAdmin); err != nil {
		t.Fatalf("orgs.AddMember(orgadmin) error = %v", err)
	}
	return f
}

// authorizer returns an authorizer over the fixture in the given grant mode.
func (f *listFilterFixture) authorizer(strict bool) *authorizer {
	return &authorizer{
		grants: f.grants, teams: f.teams, orgs: f.orgs, strict: strict,
		orgOwners: OrgResolverFunc(func(_ context.Context, id string) (string, bool) {
			f.resolves.Add(1)
			owner, ok := f.owners[id]
			return owner, ok
		}),
		templateOwners: func(context.Context) (map[string]string, error) {
			f.ownerReads.Add(1)
			return f.owners, nil
		},
	}
}

// lfSchedules returns one schedule per template plus an inline schedule for each organization and
// one owned by none.
func lfSchedules() []*schedule.Schedule {
	var out []*schedule.Schedule
	for _, tpl := range lfTemplates {
		out = append(out, &schedule.Schedule{ID: "sch_" + strings.TrimPrefix(tpl, "tpl_"), TemplateID: tpl})
	}
	for _, orgID := range []string{"org_a", "org_b", ""} {
		out = append(out, &schedule.Schedule{ID: "sch_inline_" + orgID, OrgID: orgID})
	}
	return out
}

// TestTemplateScopedFilterMatchesTheByIDRule proves the set filter the schedule and trigger lists
// use keeps exactly the rows the by-id rule allows, for every caller in both grant modes, and that
// it reads the grant table once rather than once per row.
func TestTemplateScopedFilterMatchesTheByIDRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Actor  Actor
		Strict bool
	}{}
	actors := []Actor{
		{UserID: "user_admin", Role: user.RoleAdmin},
		{UserID: lfHolder, Role: user.RoleOperator},
		{UserID: lfMember, Role: user.RoleOperator},
		{UserID: lfOrgAdmin, Role: user.RoleViewer},
		{UserID: lfOutsider, Role: user.RoleOperator},
		{Role: user.RoleOperator},
	}
	for _, strict := range []bool{false, true} {
		for _, a := range actors {
			tests = append(tests, struct {
				Actor  Actor
				Strict bool
			}{Actor: a, Strict: strict})
		}
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s strict=%v", testNum, test.Actor.UserID, test.Strict), func(t *testing.T) {
			t.Parallel()
			f := newListFilterFixture(t)
			authz := f.authorizer(test.Strict)
			ctx := context.WithValue(context.Background(), actorKey{}, test.Actor)

			var wantSched, wantTrig []string
			for _, sc := range lfSchedules() {
				if authz.authorizeSchedule(ctx, grant.AccessUse, sc) == nil {
					wantSched = append(wantSched, sc.ID)
				}
			}
			for _, tpl := range append([]string{""}, lfTemplates...) {
				if authz.authorizeAll(ctx, grant.AccessUse, tpl) == nil {
					wantTrig = append(wantTrig, "tg_"+tpl)
				}
			}

			f.grants.lists.Store(0)
			f.grants.perObject.Store(0)
			f.resolves.Store(0)
			f.ownerReads.Store(0)
			keep, err := authz.templateScopedFilter(ctx)
			if err != nil {
				t.Fatalf("templateScopedFilter() error = %v", err)
			}
			var gotSched, gotTrig []string
			for _, sc := range lfSchedules() {
				if keep(sc.TemplateID, sc.OrgID) {
					gotSched = append(gotSched, sc.ID)
				}
			}
			for _, tpl := range append([]string{""}, lfTemplates...) {
				if tpl == "" || keep(tpl, "") {
					gotTrig = append(gotTrig, "tg_"+tpl)
				}
			}
			if diff := cmp.Diff(wantSched, gotSched, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("schedules mismatch (-by id +list):\n%s", diff)
			}
			if diff := cmp.Diff(wantTrig, gotTrig, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("triggers mismatch (-by id +list):\n%s", diff)
			}
			if n := f.grants.perObject.Load(); n != 0 {
				t.Errorf("filter read grants per object %d times, want 0", n)
			}
			if n := f.grants.lists.Load(); n > 1 {
				t.Errorf("filter listed the grant table %d times, want at most 1", n)
			}
			if n := f.resolves.Load(); n != 0 {
				t.Errorf("filter resolved owners one object at a time %d times, want 0", n)
			}
			if n := f.ownerReads.Load(); n > 1 {
				t.Errorf("filter read template owners %d times, want at most 1", n)
			}
		})
	}
}

// TestTemplateScopedFilterMatrixIsNotVacuous guards the equivalence test against a fixture where
// every caller sees everything or nothing: in strict mode each non-admin caller must be both allowed
// and denied something, and the org member and the outsider must differ.
func TestTemplateScopedFilterMatrixIsNotVacuous(t *testing.T) {
	t.Parallel()
	f := newListFilterFixture(t)
	authz := f.authorizer(true)
	seen := map[string][]string{}
	for _, id := range []string{lfHolder, lfMember, lfOutsider} {
		ctx := context.WithValue(context.Background(), actorKey{},
			Actor{UserID: id, Role: user.RoleOperator})
		keep, err := authz.templateScopedFilter(ctx)
		if err != nil {
			t.Fatalf("templateScopedFilter(%s) error = %v", id, err)
		}
		seen[id] = []string{}
		for _, sc := range lfSchedules() {
			if keep(sc.TemplateID, sc.OrgID) {
				seen[id] = append(seen[id], sc.ID)
			}
		}
	}
	want := map[string][]string{
		lfHolder:   {"sch_direct", "sch_team"},
		lfMember:   {"sch_org_a", "sch_org_a_granted", "sch_org_grant", "sch_inline_org_a"},
		lfOutsider: nil,
	}
	for _, v := range seen {
		sort.Strings(v)
	}
	for _, v := range want {
		sort.Strings(v)
	}
	if diff := cmp.Diff(want, seen, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("strict visibility mismatch (-want +got):\n%s", diff)
	}
}

// TestScheduleAndTriggerListsWithholdWhatTheFetchRefuses is the disclosure the per-row lists had
// on an install without strict grants: a template granted to somebody else hid nothing from the
// schedule and trigger lists, because the lists only filtered for callers restricted on run
// objects, while fetching the same schedule answered 403. It runs through the server's own wiring,
// so the org member's case also proves template owners reach the filter.
func TestScheduleAndTriggerListsWithholdWhatTheFetchRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	grants := grant.NewMemStore()
	schedules := schedule.NewMemStore()
	triggers := trigger.NewMemStore()
	orgs := org.NewMemStore()
	templates := template.NewMemStore()
	bearer := func(name string) (string, string) {
		t.Helper()
		u, err := user.New(name, "pw-"+name, user.RoleOperator)
		if err != nil {
			t.Fatalf("user.New(%s) error = %v", name, err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save(%s) error = %v", name, err)
		}
		plain, tok, err := auth.New("t-" + name)
		if err != nil {
			t.Fatalf("auth.New(%s) error = %v", name, err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save(%s) error = %v", name, err)
		}
		return plain, u.ID
	}
	outsider, _ := bearer("outsider")
	holder, holderID := bearer("holder")
	member, memberID := bearer("member")
	// tpl_owned belongs to org_a and carries a grant to the holder, so it is access controlled and
	// the member reaches it only through the organization that owns it.
	for _, g := range []*grant.Grant{
		{ID: "g1", Subject: holderID, Object: "tpl_secret", Access: grant.AccessUse},
		{ID: "g2", Subject: holderID, Object: "tpl_owned", Access: grant.AccessUse},
	} {
		if err := grants.Save(ctx, g); err != nil {
			t.Fatalf("grants.Save(%s) error = %v", g.ID, err)
		}
	}
	if err := orgs.Save(ctx, &org.Org{ID: "org_a", Name: "a"}); err != nil {
		t.Fatalf("orgs.Save() error = %v", err)
	}
	if err := orgs.AddMember(ctx, "org_a", memberID, org.RoleMember); err != nil {
		t.Fatalf("orgs.AddMember() error = %v", err)
	}
	if err := templates.Save(ctx, &template.Template{ID: "tpl_owned", Name: "owned",
		Playbook: "site.yml", OrgID: "org_a"}); err != nil {
		t.Fatalf("templates.Save() error = %v", err)
	}
	created := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	next := created.Add(time.Hour)
	for _, sc := range []*schedule.Schedule{
		{ID: "sch_secret", Name: "secret", Cron: "0 3 * * *", TemplateID: "tpl_secret", Enabled: true,
			CreatedAt: created, NextRunAt: &next},
		{ID: "sch_open", Name: "open", Cron: "0 4 * * *", TemplateID: "tpl_open", Enabled: true,
			CreatedAt: created.Add(time.Second), NextRunAt: &next},
		{ID: "sch_owned", Name: "owned", Cron: "0 5 * * *", TemplateID: "tpl_owned", Enabled: true,
			CreatedAt: created.Add(2 * time.Second), NextRunAt: &next},
	} {
		if err := schedules.Save(ctx, sc); err != nil {
			t.Fatalf("schedules.Save(%s) error = %v", sc.ID, err)
		}
	}
	for _, tg := range []*trigger.Trigger{
		{ID: "tg_secret", Name: "secret", TemplateID: "tpl_secret", CreatedAt: created},
		{ID: "tg_open", Name: "open", TemplateID: "tpl_open", CreatedAt: created.Add(time.Second)},
		{ID: "tg_owned", Name: "owned", TemplateID: "tpl_owned", CreatedAt: created.Add(2 * time.Second)},
	} {
		if err := triggers.Save(ctx, tg); err != nil {
			t.Fatalf("triggers.Save(%s) error = %v", tg.ID, err)
		}
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens),
		WithUsers(users), WithOrgs(orgs), WithTemplates(templates), WithGrants(grants, false),
		WithSchedules(schedules), WithTriggers(triggers, nil)).Handler()
	get := func(t *testing.T, token, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	ids := func(t *testing.T, rec *httptest.ResponseRecorder, key string) []string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("list answered %d: %s", rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body[key], &rows); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		var out []string
		for _, row := range rows {
			out = append(out, row.ID)
		}
		sort.Strings(out)
		return out
	}

	tests := []struct {
		WantFetch    map[string]int
		Token        string
		WantSchedule []string
		WantTrigger  []string
	}{{ // Test 0: The outsider sees only the row nobody granted, and the fetch refuses the others.
		Token: outsider, WantSchedule: []string{"sch_open"}, WantTrigger: []string{"tg_open"},
		WantFetch: map[string]int{"sch_secret": http.StatusForbidden, "sch_owned": http.StatusForbidden},
	}, { // Test 1: The holder of both grants sees every row.
		Token: holder, WantSchedule: []string{"sch_open", "sch_owned", "sch_secret"},
		WantTrigger: []string{"tg_open", "tg_owned", "tg_secret"},
		WantFetch:   map[string]int{"sch_secret": http.StatusOK, "sch_owned": http.StatusOK},
	}, { // Test 2: The org member sees its organization's template through the server's own wiring.
		Token: member, WantSchedule: []string{"sch_open", "sch_owned"},
		WantTrigger: []string{"tg_open", "tg_owned"},
		WantFetch:   map[string]int{"sch_secret": http.StatusForbidden, "sch_owned": http.StatusOK},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantSchedule, ids(t, get(t, test.Token, "/v1/schedules"), "schedules")); diff != "" {
				t.Errorf("schedule list mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantTrigger, ids(t, get(t, test.Token, "/v1/triggers"), "triggers")); diff != "" {
				t.Errorf("trigger list mismatch (-want +got):\n%s", diff)
			}
			for id, want := range test.WantFetch {
				if code := get(t, test.Token, "/v1/schedules/"+id).Code; code != want {
					t.Errorf("fetch %s answered %d, want %d", id, code, want)
				}
			}
		})
	}
}

// listedTemplates serves a fixed template list while Get reads the full store, the way a template
// created after the doctor read its list is visible to Get and absent from the list.
type listedTemplates struct {
	template.Store
	// listed is what List returns.
	listed []*template.Template
	// gets counts Get calls.
	gets atomic.Int64
}

// List returns the fixed list.
func (l *listedTemplates) List(context.Context) ([]*template.Template, error) { return l.listed, nil }

// Get counts the call and delegates.
func (l *listedTemplates) Get(ctx context.Context, id string) (*template.Template, error) {
	l.gets.Add(1)
	return l.Store.Get(ctx, id)
}

// TestDoctorLooksSchedulesUpInItsTemplateList proves the doctor finds a schedule's template in the
// list it already read, and reads the store only for a template the list does not hold, so one
// created after the list was read is not reported missing.
func TestDoctorLooksSchedulesUpInItsTemplateList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := template.NewMemStore()
	listedTpl := &template.Template{ID: "tpl_listed", Name: "listed", Playbook: "site.yml"}
	lateTpl := &template.Template{ID: "tpl_late", Name: "late", Playbook: "site.yml"}
	for _, tp := range []*template.Template{listedTpl, lateTpl} {
		if err := store.Save(ctx, tp); err != nil {
			t.Fatalf("templates.Save(%s) error = %v", tp.ID, err)
		}
	}
	templates := &listedTemplates{Store: store, listed: []*template.Template{listedTpl}}
	schedules := schedule.NewMemStore()
	created := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	for i, tpl := range []string{"tpl_listed", "tpl_listed", "tpl_late", "tpl_gone"} {
		sc := &schedule.Schedule{ID: fmt.Sprintf("sch_%d", i), Name: tpl, Cron: "0 3 * * *",
			TemplateID: tpl, CreatedAt: created.Add(time.Duration(i) * time.Second)}
		if err := schedules.Save(ctx, sc); err != nil {
			t.Fatalf("schedules.Save(%s) error = %v", sc.ID, err)
		}
	}
	rec := httptest.NewRecorder()
	doctorHandler(templates, schedules, nil, nil, nil, nil, nil, nil, zap.NewNop()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
	var report doctorReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v: %s", err, rec.Body.String())
	}
	var missing []string
	for _, f := range report.Findings {
		if f.ObjectType == "schedule" && strings.Contains(f.Problem, "which does not exist") {
			missing = append(missing, f.ObjectID)
		}
	}
	if diff := cmp.Diff([]string{"sch_3"}, missing); diff != "" {
		t.Errorf("missing-template findings mismatch (-want +got):\n%s", diff)
	}
	if n := templates.gets.Load(); n != 2 {
		t.Errorf("doctor read templates by id %d times, want 2 (the late one and the gone one)", n)
	}
}
