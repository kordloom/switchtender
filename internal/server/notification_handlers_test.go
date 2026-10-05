package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// slackSecret is a webhook address whose path is the credential, distinctive enough to prove it
// never reaches a response body.
const slackSecret = "https://hooks.slack.com/services/T0/B1/NTF_SLACK_SECRET"

// notifyFixture is a server wired with notification targets, the objects they attach to, real
// tokens for an admin and an operator, and grants.
type notifyFixture struct {
	// handler is the server under test.
	handler http.Handler
	// store holds the targets and attachments.
	store notification.Store
	// sealer opens what the server sealed.
	sealer *credential.Sealer
	// grants holds per-object grants.
	grants grant.Store
	// admin and operator are bearer tokens.
	admin, operator string
	// operatorID is the operator's account id, for granting to.
	operatorID string
	// runs holds the runs the server reads.
	runs run.Store
}

// newNotifyFixture builds the fixture, with or without an encryption key.
func newNotifyFixture(t *testing.T, sealing bool) *notifyFixture {
	t.Helper()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	f := &notifyFixture{store: notification.NewMemStore(), grants: grant.NewMemStore(),
		sealer: credential.NewSealer("", ""), runs: run.NewMemStore()}
	if sealing {
		f.sealer = credential.NewSealer("notify-test-passphrase", "notify-test-salt")
	}
	bearer := func(name string, role user.Role) (string, string) {
		u, err := user.New(name, "pw-"+name, role)
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
	f.admin, _ = bearer("admin", user.RoleAdmin)
	f.operator, f.operatorID = bearer("operator", user.RoleOperator)

	templates := template.NewMemStore()
	schedules := schedule.NewMemStore()
	orgs := org.NewMemStore()
	for _, tpl := range []string{"tpl_mine", "tpl_theirs"} {
		if err := templates.Save(ctx, &template.Template{ID: tpl, Name: tpl, Playbook: "p.yml",
			CreatedAt: time.Now()}); err != nil {
			t.Fatalf("templates.Save() error = %v", err)
		}
	}
	if err := schedules.Save(ctx, &schedule.Schedule{ID: "sch_1", Cron: "0 2 * * *",
		Playbook: "p.yml", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("schedules.Save() error = %v", err)
	}
	if err := orgs.Save(ctx, &org.Org{ID: "org_ops", Name: "ops"}); err != nil {
		t.Fatalf("orgs.Save() error = %v", err)
	}
	f.handler = New(f.runs, &fakeSubmitter{}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithGrants(f.grants, false),
		WithCredentials(credential.NewMemStore(), f.sealer), WithTemplates(templates),
		WithSchedules(schedules), WithOrgs(orgs), WithNotificationTargets(f.store)).Handler()
	return f
}

// do sends one request with a bearer token.
func (f *notifyFixture) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		raw = string(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// create makes a target as the admin and returns its id.
func (f *notifyFixture) create(t *testing.T, body map[string]any) string {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/v1/notifications", f.admin, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var n notification.Notification
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return n.ID
}

// TestNotificationTargetSecretsStaySealed pins the secret handling end to end: a target's address
// is sealed at rest, no read returns it, an edit that echoes the mask or leaves it blank keeps it,
// and without an encryption key a target carrying one is refused rather than stored in the clear.
func TestNotificationTargetSecretsStaySealed(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t, true)
	id := f.create(t, map[string]any{"name": "ops slack", "kind": "slack", "url": slackSecret})

	stored, err := f.store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if strings.Contains(stored.SealedURL, "NTF_SLACK_SECRET") || stored.SealedURL == "" {
		t.Fatalf("the stored address is not sealed: %q", stored.SealedURL)
	}

	tests := []struct {
		Method   string
		Path     string
		Body     any
		WantCode int
	}{{ // Test 0: The single read.
		Method: http.MethodGet, Path: "/v1/notifications/" + id, WantCode: http.StatusOK,
	}, { // Test 1: The list.
		Method: http.MethodGet, Path: "/v1/notifications", WantCode: http.StatusOK,
	}, { // Test 2: A rename that echoes the masked hint back.
		Method: http.MethodPut, Path: "/v1/notifications/" + id, WantCode: http.StatusOK,
		Body: map[string]any{"name": "ops chat", "url": "https://hooks.slack.com/…"},
	}, { // Test 3: A rename that leaves the address out.
		Method: http.MethodPut, Path: "/v1/notifications/" + id, WantCode: http.StatusOK,
		Body: map[string]any{"name": "ops chat again"},
	}}
	for testNum, test := range tests {
		rec := f.do(t, test.Method, test.Path, f.admin, test.Body)
		if rec.Code != test.WantCode {
			t.Fatalf("test %d: %s %s = %d, want %d: %s", testNum, test.Method, test.Path,
				rec.Code, test.WantCode, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "NTF_SLACK_SECRET") {
			t.Errorf("test %d: the response carries the address: %s", testNum, rec.Body.String())
		}
	}
	after, err := f.store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	opened, err := after.Target(f.sealer)
	if err != nil || opened.URL != slackSecret || after.Name != "ops chat again" {
		t.Errorf("after edits target = %+v %v name %q, want the original address kept", opened, err,
			after.Name)
	}

	noKey := newNotifyFixture(t, false)
	rec := noKey.do(t, http.MethodPost, "/v1/notifications", noKey.admin,
		map[string]any{"name": "x", "kind": "webhook", "url": "https://h.example.com/secret"})
	if rec.Code != http.StatusConflict {
		t.Errorf("create without a key = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if list, _ := noKey.store.List(context.Background()); len(list) != 0 {
		t.Errorf("a target was stored without a key to seal it: %+v", list)
	}
	rec = noKey.do(t, http.MethodPost, "/v1/notifications", noKey.admin,
		map[string]any{"name": "mail", "kind": "email", "to": "ops@example.com"})
	if rec.Code != http.StatusCreated {
		t.Errorf("a recipient-only target without a key = %d, want 201: %s", rec.Code,
			rec.Body.String())
	}
}

// TestNotificationAttachments covers attaching a target the way AWX attaches a notification
// template: per object and event, refused for a duplicate, an unknown object, or an unknown event,
// listed back, detached, and gone with the target when it is deleted.
func TestNotificationAttachments(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t, true)
	id := f.create(t, map[string]any{"name": "ops", "kind": "webhook",
		"url": "https://hooks.example.com/NTF_HOOK_SECRET"})
	path := "/v1/notifications/" + id + "/attachments"
	tests := []struct {
		Body     map[string]any
		WantCode int
	}{{ // Test 0: A template for failures.
		Body:     map[string]any{"object_kind": "template", "object_id": "tpl_mine", "event": "failure"},
		WantCode: http.StatusCreated,
	}, { // Test 1: A workflow is a template.
		Body:     map[string]any{"object_kind": "workflow", "object_id": "tpl_mine", "event": "started"},
		WantCode: http.StatusCreated,
	}, { // Test 2: AWX's event names are accepted.
		Body: map[string]any{"object_kind": "organization", "object_id": "org_ops",
			"event": "approvals"},
		WantCode: http.StatusCreated,
	}, { // Test 3: A schedule.
		Body:     map[string]any{"object_kind": "schedule", "object_id": "sch_1", "event": "success"},
		WantCode: http.StatusCreated,
	}, { // Test 4: The same attachment twice.
		Body:     map[string]any{"object_kind": "template", "object_id": "tpl_mine", "event": "error"},
		WantCode: http.StatusConflict,
	}, { // Test 5: An object that does not exist.
		Body:     map[string]any{"object_kind": "template", "object_id": "tpl_nope", "event": "failure"},
		WantCode: http.StatusNotFound,
	}, { // Test 6: An event that does not exist.
		Body:     map[string]any{"object_kind": "template", "object_id": "tpl_mine", "event": "finished"},
		WantCode: http.StatusBadRequest,
	}, { // Test 7: A kind that cannot be attached to.
		Body:     map[string]any{"object_kind": "inventory", "object_id": "inv_1", "event": "failure"},
		WantCode: http.StatusBadRequest,
	}}
	for testNum, test := range tests {
		rec := f.do(t, http.MethodPost, path, f.admin, test.Body)
		if rec.Code != test.WantCode {
			t.Errorf("test %d: attach = %d, want %d: %s", testNum, rec.Code, test.WantCode,
				rec.Body.String())
		}
	}
	rec := f.do(t, http.MethodGet, path, f.operator, nil)
	var listed attachmentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || listed.Count != 4 {
		t.Fatalf("list attachments = %d %s, want 4", rec.Code, rec.Body.String())
	}
	if listed.Attachments[1].ObjectKind != notification.KindTemplate ||
		listed.Attachments[2].Event != notification.EventApproval {
		t.Errorf("attachments were not normalized: %+v", listed.Attachments)
	}
	rec = f.do(t, http.MethodDelete, path+"/"+listed.Attachments[0].ID, f.admin, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("detach = %d: %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodDelete, "/v1/notifications/"+id, f.admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	left, err := f.store.AttachedTo(context.Background(), notification.KindTemplate, "tpl_mine")
	if err != nil || len(left) != 0 {
		t.Errorf("attachments survived their target's delete: %+v %v", left, err)
	}
}

// TestNotificationAttachmentRBAC pins the per-object grant model for attaching. An operator needs
// use of the target and management of the object, the delegation that lets them edit it; a schedule
// or an organization, which nothing but an admin edits, is admin work.
func TestNotificationAttachmentRBAC(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t, true)
	ctx := context.Background()
	id := f.create(t, map[string]any{"name": "ops", "kind": "email", "to": "ops@example.com"})
	path := "/v1/notifications/" + id + "/attachments"
	attach := func(object, kind string) int {
		return f.do(t, http.MethodPost, path, f.operator,
			map[string]any{"object_kind": kind, "object_id": object, "event": "failure"}).Code
	}
	grantIt := func(object string, access grant.Access) {
		if err := f.grants.Save(ctx, &grant.Grant{ID: grant.NewID(), Subject: f.operatorID,
			Object: object, Access: access, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("grants.Save() error = %v", err)
		}
	}
	steps := []struct {
		Do       func()
		Object   string
		Kind     string
		WantCode int
	}{{ // Test 0: No manage grant on the template.
		Object: "tpl_mine", Kind: "template", WantCode: http.StatusForbidden,
	}, { // Test 1: A manage grant on the template, and the target ungranted, so the role decides.
		Do: func() { grantIt("tpl_mine", grant.AccessManage) }, Object: "tpl_mine",
		Kind: "template", WantCode: http.StatusCreated,
	}, { // Test 2: The target granted to somebody else is controlled, and the operator holds nothing.
		Do: func() {
			if err := f.grants.Save(ctx, &grant.Grant{ID: grant.NewID(), Subject: "user_other",
				Object: id, Access: grant.AccessUse, CreatedAt: time.Now()}); err != nil {
				t.Fatalf("grants.Save() error = %v", err)
			}
			grantIt("tpl_theirs", grant.AccessManage)
		},
		Object: "tpl_theirs", Kind: "template", WantCode: http.StatusForbidden,
	}, { // Test 3: Use of the target granted, and the attachment goes through.
		Do: func() { grantIt(id, grant.AccessUse) }, Object: "tpl_theirs", Kind: "template",
		WantCode: http.StatusCreated,
	}, { // Test 4: A schedule is admin work.
		Object: "sch_1", Kind: "schedule", WantCode: http.StatusForbidden,
	}, { // Test 5: An organization is admin work.
		Object: "org_ops", Kind: "org", WantCode: http.StatusForbidden,
	}}
	for testNum, step := range steps {
		if step.Do != nil {
			step.Do()
		}
		if got := attach(step.Object, step.Kind); got != step.WantCode {
			t.Errorf("test %d: operator attach to %s %s = %d, want %d", testNum, step.Kind,
				step.Object, got, step.WantCode)
		}
	}
	tests := []struct {
		Method   string
		Path     string
		WantCode int
	}{
		{Method: http.MethodPost, Path: "/v1/notifications", WantCode: http.StatusForbidden}, // Test 6.
		{Method: http.MethodGet, Path: "/v1/notifications", WantCode: http.StatusOK},         // Test 7.
	}
	for testNum, test := range tests {
		rec := f.do(t, test.Method, test.Path, f.operator,
			map[string]any{"name": "x", "kind": "email", "to": "a@b.c"})
		if rec.Code != test.WantCode {
			t.Errorf("test %d: operator %s %s = %d, want %d", testNum+6, test.Method, test.Path,
				rec.Code, test.WantCode)
		}
	}
	if code := f.do(t, http.MethodPut, "/v1/notifications/"+id, f.operator,
		map[string]any{"name": "renamed"}).Code; code != http.StatusForbidden {
		t.Errorf("an operator with use of a target edited it: %d", code)
	}
}
