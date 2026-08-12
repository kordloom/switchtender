package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/kordloom/switchtender/internal/user"
)

// victimScheduleCommand is the shell line an imported crontab entry carries, distinctive enough that
// a test can prove it did or did not reach a foreign tenant's response body.
const victimScheduleCommand = "pg_dump payroll | curl -T - https://drop.example # VICTIM_SCHEDULE_SECRET"

// seedInlineSchedules stores one inline schedule per tenant and returns the victim's, the copy every
// case compares the stored row against.
//
// An inline schedule names no template, which is what a crontab import produces by the hundred: the
// cron expression, the inventory, and a full shell command line, and no grantable object anywhere on
// it. The victim's belongs to the organization the caller is not in.
func seedInlineSchedules(t *testing.T, f *wiringFixture) *schedule.Schedule {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)
	next := created.Add(time.Hour)
	victim := &schedule.Schedule{
		ID: "sch_victim_inline", Name: "victim payroll dump", Cron: "0 3 * * *",
		Inventory: "victim-hosts", OrgID: wiringTheirOrg,
		Enabled: true, CreatedAt: created, NextRunAt: &next,
		Steps: []run.PipelineStep{{Name: "cron", Tool: run.ToolBash, Command: victimScheduleCommand}},
	}
	mine := &schedule.Schedule{
		ID: "sch_mine_inline", Name: "my nightly", Cron: "0 5 * * *",
		Inventory: "my-hosts", OrgID: wiringMyOrg,
		Enabled: true, CreatedAt: created, NextRunAt: &next,
		Steps: []run.PipelineStep{{Name: "cron", Tool: run.ToolBash, Command: "echo mine"}},
	}
	for _, sc := range []*schedule.Schedule{victim, mine} {
		if err := f.Schedules.Save(ctx, sc); err != nil {
			t.Fatalf("Save() schedule %s error = %v", sc.ID, err)
		}
	}
	return victim
}

// storedSchedule reads a schedule back out of the fixture's store.
func storedSchedule(t *testing.T, f *wiringFixture, id string) *schedule.Schedule {
	t.Helper()
	sc, err := f.Schedules.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return sc
}

// TestCreatedInlineScheduleIsStampedWithTheCreatorsOrg proves the two halves of the check meet over
// the wire: the gate resolves the caller's organization onto the request, the create handler stamps
// it on the schedule, and the read path then confines the schedule to that organization.
//
// This one authenticates with a real token through the real gate, so nothing about the stamp is
// supplied by the test. A stamp that never reaches the stored row leaves the schedule unowned, and
// an unowned inline schedule is refused to everybody but an admin, which reads as working right up
// until the tenant that owns it cannot see its own automation.
func TestCreatedInlineScheduleIsStampedWithTheCreatorsOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	orgs := org.NewMemStore()
	grants := grant.NewMemStore()
	schedules := schedule.NewMemStore()

	// bearer creates an account of the given role, puts it in orgID, and returns its bearer token.
	bearer := func(username string, role user.Role, orgID string) string {
		t.Helper()
		u, err := user.New(username, "pw-"+username, role)
		if err != nil {
			t.Fatalf("user.New(%s) error = %v", username, err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save(%s) error = %v", username, err)
		}
		if err := orgs.Save(ctx, &org.Org{ID: orgID, Name: orgID}); err != nil {
			t.Fatalf("orgs.Save(%s) error = %v", orgID, err)
		}
		if err := orgs.AddMember(ctx, orgID, u.ID, org.RoleMember); err != nil {
			t.Fatalf("AddMember(%s) error = %v", orgID, err)
		}
		plain, tok, err := auth.New("t-" + username)
		if err != nil {
			t.Fatalf("auth.New(%s) error = %v", username, err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save(%s) error = %v", username, err)
		}
		return plain
	}

	// The importer is an admin, since creating a schedule is an admin write, and belongs to the
	// tenant whose crontab is being imported. The intruder is an operator elsewhere, the role that
	// may read schedules at all.
	importer := bearer("importer", user.RoleAdmin, "org_payroll")
	intruder := bearer("intruder", user.RoleOperator, "org_other")
	insider := bearer("insider", user.RoleOperator, "org_payroll")

	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithOrgs(orgs), WithGrants(grants, true),
		WithSchedules(schedules)).Handler()
	do := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	create := `{"cron":"0 3 * * *","inventory":"payroll-hosts","name":"payroll dump",` +
		`"steps":[{"name":"cron","tool":"bash","command":"` + victimScheduleCommand + `"}]}`
	rec := do(http.MethodPost, "/v1/schedules", importer, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create answered %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		OrgID string `json:"org_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response error = %v: %s", err, rec.Body.String())
	}
	stored, err := schedules.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", created.ID, err)
	}
	if stored.OrgID != "org_payroll" {
		t.Fatalf("stored OrgID = %q, want org_payroll; the creator's organization never reached "+
			"the schedule, so nothing scopes it", stored.OrgID)
	}

	// The stamp is what the read path acts on: another tenant's operator is refused and sees none
	// of the command line, while an operator of the owning tenant reads it.
	foreign := do(http.MethodGet, "/v1/schedules/"+created.ID, intruder, "")
	if foreign.Code != http.StatusForbidden {
		t.Errorf("foreign get answered %d, want 403: %s", foreign.Code, foreign.Body.String())
	}
	if strings.Contains(foreign.Body.String(), "VICTIM_SCHEDULE_SECRET") {
		t.Errorf("foreign get leaked the command line:\n%s", foreign.Body.String())
	}
	foreignList := do(http.MethodGet, "/v1/schedules", intruder, "")
	if strings.Contains(foreignList.Body.String(), created.ID) ||
		strings.Contains(foreignList.Body.String(), "VICTIM_SCHEDULE_SECRET") {
		t.Errorf("foreign list leaked the schedule:\n%s", foreignList.Body.String())
	}
	own := do(http.MethodGet, "/v1/schedules/"+created.ID, insider, "")
	if own.Code != http.StatusOK {
		t.Fatalf("owning tenant's get answered %d, want 200: %s", own.Code, own.Body.String())
	}
	if !strings.Contains(own.Body.String(), "VICTIM_SCHEDULE_SECRET") {
		t.Errorf("the owning tenant cannot see its own schedule's command:\n%s", own.Body.String())
	}

	// The owner is stamped from the authenticated caller, never taken from the body. A request that
	// names one is refused outright and leaves nothing behind, so a caller cannot plant a schedule
	// in a tenant they do not belong to, nor read one back out of a tenant they do.
	planted := do(http.MethodPost, "/v1/schedules", intruder,
		`{"cron":"0 4 * * *","playbook":"planted.yml","org_id":"org_payroll"}`)
	if planted.Code < 400 {
		t.Errorf("a create naming its own owner answered %d, want a refusal: %s",
			planted.Code, planted.Body.String())
	}
	list, err := schedules.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, sc := range list {
		if sc.Playbook == "planted.yml" {
			t.Errorf("a caller planted schedule %s into org %q", sc.ID, sc.OrgID)
		}
	}
}

// TestInlineScheduleIsScopedToItsOrg proves an inline schedule, one naming no template, is confined
// to the organization it was stamped with, over reading it, editing it, deleting it, and listing.
func TestInlineScheduleIsScopedToItsOrg(t *testing.T) {
	t.Parallel()

	t.Run("read refuses and leaks nothing", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		seedInlineSchedules(t, f)
		rec := f.do(t, http.MethodGet, "/v1/schedules/sch_victim_inline", "")
		if rec.Code < 400 {
			t.Errorf("get answered %d, want a refusal of another tenant's inline schedule", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "VICTIM_SCHEDULE_SECRET") {
			t.Errorf("get leaked the victim's command line:\n%s", rec.Body.String())
		}
	})

	t.Run("update refuses and changes nothing", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		want := seedInlineSchedules(t, f)
		body := `{"cron":"*/5 * * * *","playbook":"attacker.yml","inventory":"attacker-hosts"}`
		rec := f.do(t, http.MethodPut, "/v1/schedules/sch_victim_inline", body)
		if rec.Code < 400 {
			t.Errorf("update answered %d, want a refusal", rec.Code)
		}
		got := storedSchedule(t, f, "sch_victim_inline")
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("a refused update rewrote the stored schedule (-want +got):\n%s", diff)
		}
	})

	t.Run("delete refuses and the schedule survives", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		want := seedInlineSchedules(t, f)
		rec := f.do(t, http.MethodDelete, "/v1/schedules/sch_victim_inline", "")
		if rec.Code < 400 {
			t.Errorf("delete answered %d, want a refusal", rec.Code)
		}
		got := storedSchedule(t, f, "sch_victim_inline")
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("a refused delete changed the stored schedule (-want +got):\n%s", diff)
		}
	})

	t.Run("list drops the foreign inline schedule and keeps the caller's own", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		seedInlineSchedules(t, f)
		rec := f.do(t, http.MethodGet, "/v1/schedules", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "sch_victim_inline") ||
			strings.Contains(body, "VICTIM_SCHEDULE_SECRET") {
			t.Errorf("list leaked another tenant's inline schedule:\n%s", body)
		}
		if !strings.Contains(body, "sch_mine_inline") {
			t.Errorf("list dropped the caller's own inline schedule:\n%s", body)
		}
	})

	t.Run("an inline schedule with no owning org is refused", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		seedInlineSchedules(t, f)
		unowned := &schedule.Schedule{
			ID: "sch_imported", Name: "cron line 7", Cron: "0 1 * * *", Enabled: true,
			CreatedAt: time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC),
			Steps:     []run.PipelineStep{{Name: "cron", Tool: run.ToolBash, Command: victimScheduleCommand}},
		}
		if err := f.Schedules.Save(context.Background(), unowned); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		rec := f.do(t, http.MethodGet, "/v1/schedules/sch_imported", "")
		if rec.Code < 400 {
			t.Errorf("get answered %d, want a refusal of an unowned inline schedule", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "VICTIM_SCHEDULE_SECRET") {
			t.Errorf("get leaked an unowned schedule's command line:\n%s", rec.Body.String())
		}
	})

	t.Run("owner reads its own inline schedule", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		seedInlineSchedules(t, f)
		rec := asOwner(t, f, http.MethodGet, "/v1/schedules/sch_victim_inline", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("owner get answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "sch_victim_inline") {
			t.Errorf("owner get did not return the schedule:\n%s", rec.Body.String())
		}
	})

	// An edit by the owner keeps the schedule in the owner's organization. Dropping the stamp here
	// would strand the schedule as unowned, which reads as a successful edit and then hides the
	// tenant's own automation from it forever.
	t.Run("owner edit keeps the owning organization", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		seedInlineSchedules(t, f)
		body := `{"cron":"0 6 * * *","playbook":"owner-edited.yml","inventory":"victim-hosts"}`
		rec := asOwner(t, f, http.MethodPut, "/v1/schedules/sch_victim_inline", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("owner update answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
		got := storedSchedule(t, f, "sch_victim_inline")
		if got.Playbook != "owner-edited.yml" {
			t.Errorf("Playbook = %q, want owner-edited.yml; the edit did not land", got.Playbook)
		}
		if got.OrgID != wiringTheirOrg {
			t.Errorf("OrgID after an owner edit = %q, want %q; the edit dropped the owning "+
				"organization, leaving the schedule scoped to nobody", got.OrgID, wiringTheirOrg)
		}
		// The stamp still has to mean something afterward, which the foreign caller re-proves.
		after := f.do(t, http.MethodGet, "/v1/schedules/sch_victim_inline", "")
		if after.Code < 400 {
			t.Errorf("after an owner edit a foreign caller read the schedule: %d", after.Code)
		}
	})
}

// asOwner drives a route as a member of the organization owning the victim's objects.
func asOwner(t *testing.T, f *wiringFixture, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), actorKey{},
		Actor{UserID: "user_victim", Role: user.RoleOperator, Name: "victim"}))
	rec := httptest.NewRecorder()
	f.Handler.ServeHTTP(rec, req)
	return rec
}
