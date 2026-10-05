package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheRequestFingerprintNeverCommitsASecretItCannotName pins that a secret whose field name says
// nothing never enters the keyed fingerprint the gate commits for a request. The fingerprint's key
// is stored beside the entry, so whoever holds the database can confirm a guess of anything it
// committed, and a PIN or a webhook address is short enough to guess. Each secret is replaced by a
// fixed marker, and everything around it is committed exactly as sent.
func TestTheRequestFingerprintNeverCommitsASecretItCannotName(t *testing.T) {
	t.Parallel()
	const marker = `"«withheld: secret»"`
	tests := []struct {
		// Method and Path are the request.
		Method, Path string
		// Body is what is sent, holding the secret.
		Body string
		// WantCommitted is the body the fingerprint commits, the secret replaced by the marker.
		WantCommitted string
	}{{ // Test 0: The answer to a secret survey question, beside an ordinary one that stays.
		Method: http.MethodPost, Path: "/v1/templates/tpl_pin/launch",
		Body:          `{"answers":{"admin_pin":"4417","region":"us-east-1"}}`,
		WantCommitted: `{"answers":{"admin_pin":` + marker + `,"region":"us-east-1"}}`,
	}, { // Test 1: A secret question's variable set through extra vars, which is refused after it
		// is recorded.
		Method: http.MethodPost, Path: "/v1/templates/tpl_pin/launch",
		Body:          `{"extra_vars":{"admin_pin":"4417","tier":"gold"}}`,
		WantCommitted: `{"extra_vars":{"admin_pin":` + marker + `,"tier":"gold"}}`,
	}, { // Test 2: A launch of a template the gate cannot read withholds every answer.
		Method: http.MethodPost, Path: "/v1/templates/tpl_missing/launch",
		Body:          `{"answers":{"region":"us-east-1"}}`,
		WantCommitted: `{"answers":{"region":` + marker + `}}`,
	}, { // Test 3: A secret question's default on a new template, in AWX's spelling of the type.
		Method: http.MethodPost, Path: "/v1/templates",
		Body: `{"name":"t","tool":"bash","command":"x","survey":[` +
			`{"var":"admin_pin","label":"PIN","type":"password","default":"4417"},` +
			`{"var":"region","label":"Region","type":"text","default":"us-east-1"}]}`,
		WantCommitted: `{"name":"t","tool":"bash","command":"x","survey":[` +
			`{"var":"admin_pin","label":"PIN","type":"password","default":` + marker + `},` +
			`{"var":"region","label":"Region","type":"text","default":"us-east-1"}]}`,
	}, { // Test 4: A template's own notification target address, on an update.
		Method: http.MethodPut, Path: "/v1/templates/tpl_pin",
		Body: `{"name":"t","tool":"bash","command":"x","notifications":[` +
			`{"kind":"slack","url":"https://hooks.example.com/T0/B0/abcdef"},` +
			`{"kind":"email","to":"ops@example.com"}]}`,
		WantCommitted: `{"name":"t","tool":"bash","command":"x","notifications":[` +
			`{"kind":"slack","url":` + marker + `},{"kind":"email","to":"ops@example.com"}]}`,
	}, { // Test 5: A run's own notification target key.
		Method: http.MethodPost, Path: "/v1/runs",
		Body: `{"tool":"bash","command":"true","notifications":[` +
			`{"kind":"pagerduty","key":"R0UTE1234"}]}`,
		WantCommitted: `{"tool":"bash","command":"true","notifications":[` +
			`{"kind":"pagerduty","key":` + marker + `}]}`,
	}, { // Test 6: A named target's key, on create.
		Method: http.MethodPost, Path: "/v1/notifications",
		Body:          `{"name":"pager","kind":"pagerduty","key":"R0UTE1234"}`,
		WantCommitted: `{"name":"pager","kind":"pagerduty","key":` + marker + `}`,
	}, { // Test 7: A named target's address, on update.
		Method: http.MethodPut, Path: "/v1/notifications/ntf_hook",
		Body:          `{"name":"hook","kind":"webhook","url":"https://hooks.example.com/abcdef"}`,
		WantCommitted: `{"name":"hook","kind":"webhook","url":` + marker + `}`,
	}, { // Test 8: A body with no secret in it is committed exactly as sent.
		Method: http.MethodPost, Path: "/v1/runs",
		Body:          `{"tool":"bash","command":"echo 4417"}`,
		WantCommitted: `{"tool":"bash","command":"echo 4417"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler, bearer, audits := fingerprintServer(t)
			req := httptest.NewRequest(test.Method, test.Path, strings.NewReader(test.Body))
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(httptest.NewRecorder(), req)
			chain, err := audits.Chain(context.Background())
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			var entry *audit.Entry
			for _, e := range chain {
				if e.Method == test.Method && e.Path == test.Path {
					entry = e
				}
			}
			if entry == nil || entry.ContentDigest == "" {
				t.Fatalf("the request was not recorded with a fingerprint: %+v", entry)
			}
			withheld := test.Body != test.WantCommitted
			if withheld && audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce,
				[]byte(test.Body)) {
				t.Error("the request fingerprint commits the secret as sent")
			}
			if !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce,
				[]byte(test.WantCommitted)) {
				t.Errorf("the request fingerprint does not commit %s", test.WantCommitted)
			}
		})
	}
}

// fingerprintServer builds a server whose gate records every change, holding a template tpl_pin that
// asks one secret question, admin_pin, and one ordinary one, and a named webhook target ntf_hook. It
// returns the handler, an admin's bearer token, and the audit store.
func fingerprintServer(t *testing.T) (http.Handler, string, audit.Store) {
	t.Helper()
	ctx := context.Background()
	tokens, users := auth.NewMemStore(), user.NewMemStore()
	admin, err := user.New("ops-admin", "a long enough password", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	bearer, tok, err := auth.New("laptop")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = admin.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{ID: "tpl_pin", Name: "pin", Tool: run.ToolBash,
		Command: "true", CreatedAt: time.Now(), Survey: []template.SurveyField{
			{Var: "admin_pin", Label: "PIN", Type: template.FieldSecret},
			{Var: "region", Label: "Region", Type: template.FieldText},
		}}); err != nil {
		t.Fatalf("templates.Save() error = %v", err)
	}
	sealer := secretSurveySealer()
	targets := named.NewMemStore()
	hook := &named.Notification{ID: "ntf_hook", Name: "hook", CreatedAt: time.Now()}
	if err := hook.SetTarget(run.NotifyTarget{Kind: run.NotifyWebhook,
		URL: "https://hooks.example.com/old"}, sealer); err != nil {
		t.Fatalf("SetTarget() error = %v", err)
	}
	if err := targets.Save(ctx, hook); err != nil {
		t.Fatalf("targets.Save() error = %v", err)
	}
	store, audits := run.NewMemStore(), audit.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := dispatch.New(store, runner, zap.NewNop(), dispatch.WithAudits(audits),
		dispatch.WithNoJanitor())
	t.Cleanup(d.Close)
	handler := New(store, d, zap.NewNop(), WithTokens(tokens), WithUsers(users), WithAudit(audits),
		WithTemplates(templates), WithCredentials(credential.NewMemStore(), sealer),
		WithNotificationTargets(targets)).Handler()
	return handler, bearer, audits
}
