package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// BenchmarkScheduleListRestrictedSQLite measures a restricted operator's schedule list at the
// 10-09 finding's scale on SQLite: 5,000 template-backed schedules, 11,000 grants, and a caller in
// an organization that owns half the templates.
func BenchmarkScheduleListRestrictedSQLite(b *testing.B) {
	const templates, grants = 5000, 11000
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(b.TempDir(), "switchtender.db"))
	if err != nil {
		b.Fatalf("open sqlite: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	u, err := user.New("operator", "pw-operator", user.RoleOperator)
	if err != nil {
		b.Fatalf("user.New() error = %v", err)
	}
	if err := db.Users().Save(ctx, u); err != nil {
		b.Fatalf("users.Save() error = %v", err)
	}
	plain, tok, err := auth.New("t-operator")
	if err != nil {
		b.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = u.ID
	if err := db.Tokens().Save(ctx, tok); err != nil {
		b.Fatalf("tokens.Save() error = %v", err)
	}
	for _, id := range []string{"org_a", "org_b"} {
		if err := db.Orgs().Save(ctx, &org.Org{ID: id, Name: id}); err != nil {
			b.Fatalf("orgs.Save(%s) error = %v", id, err)
		}
	}
	if err := db.Orgs().AddMember(ctx, "org_a", u.ID, org.RoleMember); err != nil {
		b.Fatalf("orgs.AddMember() error = %v", err)
	}

	created := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	next := created.Add(time.Hour)
	for i := range templates {
		owner := "org_b"
		if i%2 == 0 {
			owner = "org_a"
		}
		tpl := &template.Template{ID: fmt.Sprintf("tpl_%05d", i), Name: fmt.Sprintf("t%05d", i),
			Playbook: "site.yml", OrgID: owner, CreatedAt: created}
		if err := db.Templates().Save(ctx, tpl); err != nil {
			b.Fatalf("templates.Save(%s) error = %v", tpl.ID, err)
		}
		sc := &schedule.Schedule{ID: fmt.Sprintf("sch_%05d", i), Name: tpl.Name, Cron: "0 3 * * *",
			TemplateID: tpl.ID, Enabled: true, CreatedAt: created.Add(time.Duration(i) * time.Second),
			NextRunAt: &next}
		if err := db.Schedules().Save(ctx, sc); err != nil {
			b.Fatalf("schedules.Save(%s) error = %v", sc.ID, err)
		}
	}
	// Most grants go to somebody else, so most templates are controlled and the caller holds few.
	// The one project grant restricts the caller on a run object too, which is what made the old
	// list take its per-row path.
	for i := range grants {
		g := &grant.Grant{ID: fmt.Sprintf("grt_%05d", i), Subject: "user_other",
			Object: fmt.Sprintf("tpl_%05d", i%templates), Access: grant.AccessUse, CreatedAt: created}
		if i%100 == 1 {
			g.Subject = u.ID
		}
		if i == 0 {
			g.Object = "proj_restricting"
		}
		if err := db.Grants().Save(ctx, g); err != nil {
			b.Fatalf("grants.Save(%s) error = %v", g.ID, err)
		}
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(db.Tokens()),
		WithUsers(db.Users()), WithOrgs(db.Orgs()), WithTeams(db.Teams()),
		WithGrants(db.Grants(), false), WithTemplates(db.Templates()),
		WithSchedules(db.Schedules())).Handler()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/v1/schedules", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("list answered %d: %s", rec.Code, rec.Body.String())
		}
	}
}
