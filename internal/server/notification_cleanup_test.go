package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/template"
)

// staleAttachments is a notification store whose view of an object's attachments is empty, standing
// in for an attachment made after the gate read them and before the delete ran.
type staleAttachments struct {
	notification.Store
}

// AttachedTo reports no attachments, whatever the object holds.
func (staleAttachments) AttachedTo(context.Context, string, string) ([]*notification.Attachment,
	error) {
	return nil, nil
}

// cleanupFixture is a server on a SQLite database with a chain, a target, and one object of every
// attachable kind with the target attached to it.
type cleanupFixture struct {
	// handler is the server under test.
	handler http.Handler
	// db is the database.
	db *sqlitestore.DB
	// audits is the chain.
	audits audit.Store
	// token is an admin bearer token.
	token string
}

// newCleanupFixture builds the fixture, serving the notification store wrap returns.
func newCleanupFixture(t *testing.T, wrap func(notification.Store) notification.Store) *cleanupFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(db.Templates().Save(ctx, &template.Template{ID: "tpl_a", Name: "a", Playbook: "p.yml",
		CreatedAt: at}))
	must(db.Schedules().Save(ctx, &schedule.Schedule{ID: "sch_a", Name: "a", Cron: "0 2 * * *",
		Playbook: "p.yml", CreatedAt: at}))
	must(db.Projects().Save(ctx, &project.Project{ID: "proj_a", Name: "a",
		RepoURL: "https://git.example.com/a.git", CreatedAt: at}))
	must(db.Orgs().Save(ctx, &org.Org{ID: "org_a", Name: "a", CreatedAt: at}))
	for _, id := range []string{"ntf_x", "ntf_y"} {
		must(db.Notifications().Save(ctx, &notification.Notification{ID: id, Name: id,
			Kind: "email", To: "ops@example.com", CreatedAt: at}))
	}
	for i, object := range []struct{ kind, id string }{
		{notification.KindTemplate, "tpl_a"}, {notification.KindSchedule, "sch_a"},
		{notification.KindProject, "proj_a"}, {notification.KindOrg, "org_a"},
	} {
		for j, target := range []string{"ntf_x", "ntf_y"} {
			must(db.Notifications().Attach(ctx, &notification.Attachment{
				ID: "nta_" + object.id + target, NotificationID: target, ObjectKind: object.kind,
				ObjectID: object.id, Event: "failure",
				CreatedAt: at.Add(time.Duration(i*2+j) * time.Second)}))
		}
	}
	plain, tok, err := auth.New("admin")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	must(db.Tokens().Save(ctx, tok))
	audits := audit.NewMemStore()
	f := &cleanupFixture{db: db, audits: audits, token: plain}
	f.handler = New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTokens(db.Tokens()), WithAudit(audits), WithTemplates(db.Templates()),
		WithSchedules(db.Schedules()), WithProjects(db.Projects()), WithOrgs(db.Orgs()),
		WithNotificationTargets(wrap(db.Notifications()))).Handler()
	return f
}

// TestDeleteRecordsItsAttachmentCleanup pins decision 36 end to end on a real database: deleting a
// template, a schedule, a project, or an organization removes its notification attachments in the
// same transaction, keeps the targets, and states the cleanup, the count and the targets, inside
// the delete's own chain entry rather than as entries of its own.
func TestDeleteRecordsItsAttachmentCleanup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path     string
		Kind     string
		ObjectID string
	}{
		{Path: "/v1/templates/tpl_a", Kind: notification.KindTemplate,
			ObjectID: "tpl_a"}, // Test 0.
		{Path: "/v1/schedules/sch_a", Kind: notification.KindSchedule,
			ObjectID: "sch_a"}, // Test 1.
		{Path: "/v1/projects/proj_a", Kind: notification.KindProject,
			ObjectID: "proj_a"}, // Test 2.
		{Path: "/v1/orgs/org_a", Kind: notification.KindOrg,
			ObjectID: "org_a"}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(test.Kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newCleanupFixture(t, func(s notification.Store) notification.Store { return s })
			before, err := f.audits.Chain(ctx)
			if err != nil {
				t.Fatalf("test %d: Chain() error = %v", testNum, err)
			}
			req := httptest.NewRequest(http.MethodDelete, test.Path, nil)
			req.Header.Set("Authorization", "Bearer "+f.token)
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("test %d: DELETE = %d: %s", testNum, rec.Code, rec.Body.String())
			}
			left, err := f.db.Notifications().AttachedTo(ctx, test.Kind, test.ObjectID)
			if err != nil || len(left) != 0 {
				t.Errorf("test %d: attachments after the delete = %v, %v, want none", testNum,
					left, err)
			}
			for _, id := range []string{"ntf_x", "ntf_y"} {
				if _, err := f.db.Notifications().Get(ctx, id); err != nil {
					t.Errorf("test %d: the delete removed target %s: %v", testNum, id, err)
				}
			}
			after, err := f.audits.Chain(ctx)
			if err != nil {
				t.Fatalf("test %d: Chain() error = %v", testNum, err)
			}
			if len(after) != len(before)+1 {
				t.Errorf("test %d: the delete appended %d chain entries, want its one", testNum,
					len(after)-len(before))
			}
			entries, err := f.audits.List(ctx, 1)
			if err != nil || len(entries) != 1 {
				t.Fatalf("test %d: List() = %v, %v", testNum, entries, err)
			}
			want := test.Path + "?notification_attachments=2&notification_targets=ntf_x,ntf_y"
			if diff := cmp.Diff(want, entries[0].Path); diff != "" {
				t.Errorf("test %d: the delete's entry path mismatch (-want +got):\n%s", testNum,
					diff)
			}
			got, ok := notification.ParsePathSuffix(entries[0].Path)
			if !ok || !got.Equal(notification.Cleanup{Removed: 2,
				Targets: []string{"ntf_x", "ntf_y"}}) {
				t.Errorf("test %d: the entry's cleanup reads back as %+v, %v", testNum, got, ok)
			}
		})
	}
}

// TestDeleteRefusesAStaleCleanupRecord pins that a delete whose attachments are not the ones its
// chain entry stated deletes nothing and says so, rather than leaving the record wrong: here the
// gate read no attachments and the object holds two.
func TestDeleteRefusesAStaleCleanupRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCleanupFixture(t, func(s notification.Store) notification.Store {
		return staleAttachments{Store: s}
	})
	req := httptest.NewRequest(http.MethodDelete, "/v1/templates/tpl_a", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "nothing was deleted") {
		t.Fatalf("DELETE = %d %s, want 409 saying nothing was deleted", rec.Code,
			rec.Body.String())
	}
	if _, err := f.db.Templates().Get(ctx, "tpl_a"); err != nil {
		t.Errorf("the template was deleted after all: %v", err)
	}
	left, err := f.db.Notifications().AttachedTo(ctx, notification.KindTemplate, "tpl_a")
	if err != nil || len(left) != 2 {
		t.Errorf("attachments after the refusal = %d, %v, want both kept", len(left), err)
	}
}

// TestDoctorReportsNotificationProblems pins the doctor's notification findings: a target waiting
// for its secret, and an attachment whose object is gone, from any path, here a direct store write.
func TestDoctorReportsNotificationProblems(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCleanupFixture(t, func(s notification.Store) notification.Store { return s })
	shell := &notification.Notification{ID: "ntf_shell", Name: "imported slack", Kind: "slack",
		NeedsSecret: true, CreatedAt: time.Now()}
	if err := f.db.Notifications().Save(ctx, shell); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := f.db.Notifications().Attach(ctx, &notification.Attachment{ID: "nta_orphan",
		NotificationID: "ntf_x", ObjectKind: notification.KindTemplate, ObjectID: "tpl_gone",
		Event: "failure", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/doctor", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/doctor = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"object_id":"ntf_shell"`, "Is waiting for its secret, its address",
		`"object_id":"nta_orphan"`, "Is attached to template tpl_gone, which does not exist",
		`"notification_targets":3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the doctor's report does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"object_id":"nta_tpl_antf_x"`) {
		t.Errorf("an attachment whose object exists was reported:\n%s", body)
	}
}
