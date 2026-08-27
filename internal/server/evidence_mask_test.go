package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestRunEvidenceMasksNotificationSecrets covers the one run route that handed back the values every
// other route redacts.
//
// A notification target carries a real credential: a Slack or Teams webhook URL is a bearer secret on
// its own, and a PagerDuty routing key or Grafana token sits in the target's key field. Listing a
// template masks them, fetching a run masks them, so an operator never sees them. The evidence endpoint
// serialized the stored run whole, so asking for the same run's evidence as JSON returned every one of
// them in cleartext, and the route admits the non-admin who fired the run, which is exactly the caller
// the masking exists for. An agent acting as that actor is admitted too, and the endpoint was built for
// agents to read.
func TestRunEvidenceMasksNotificationSecrets(t *testing.T) {
	// No t.Parallel: the identity loader reads an environment variable.
	ctx := context.Background()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()

	const webhookSecret = "T000/B000/SLACKWEBHOOKSECRET"
	const routingKey = "PAGERDUTY-ROUTING-KEY-SECRET"
	// A bash, python, powershell, or go run stores its whole script in Command, and a script carries
	// whatever it was written with. The HTML dossier redacts this field; the JSON shape returned it
	// verbatim, and the JSON shape is exactly what the MCP get_run_evidence tool hands a model.
	const inlineSecret = "hunter2-inline-db-password"

	at := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	creation := &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "jane", ActorType: "session",
		Method: http.MethodPost, Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	r := &run.Run{
		ID: "run_notified", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Command: "export DB_PASSWORD=" + inlineSecret + "\npsql -c 'select 1'",
		Actor:   "jane", ActorType: "session", AuditReceipt: audit.Receipt(creation),
		CreatedAt: at, StartedAt: &at,
		Notifications: []run.NotifyTarget{
			{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/" + webhookSecret},
			{Kind: run.NotifyPagerDuty, Key: routingKey},
		},
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	ended := at.Add(time.Minute)
	r.Status, r.EndedAt = run.StatusSucceeded, &ended
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:dispatcher", nil); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	// jane: the non-admin operator who fired the run, which is who this route admits besides an admin.
	jane, err := user.New("jane", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, jane); err != nil {
		t.Fatalf("Save(user) error = %v", err)
	}
	plain, tok, err := auth.New("jane-token")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = jane.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save(token) error = %v", err)
	}
	r.Actor = tok.Name
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(actor) error = %v", err)
	}

	handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithAudit(audits), WithTokens(tokens), WithUsers(users)).Handler()

	// Both shapes of the endpoint, since the JSON one is what an agent reads and the HTML one is what a
	// person downloads, and the stored run reaches both.
	for _, path := range []string{
		"/v1/runs/run_notified/evidence?format=json",
		"/v1/runs/run_notified/evidence",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200 (body %s)", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, secret := range []struct {
			// What names the field.
			What string
			// Value must not appear.
			Value string
		}{
			{"Slack webhook", webhookSecret},
			{"PagerDuty routing key", routingKey},
			{"inline command secret", inlineSecret},
		} {
			if strings.Contains(body, secret.Value) {
				t.Errorf("%s returns the run's %s in cleartext, which every other run route masks",
					path, secret.What)
			}
		}
	}

	// The same run fetched normally is masked, which is the behavior evidence has to match rather than
	// undo.
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_notified", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), webhookSecret) {
		t.Error("the run fetch itself leaks the webhook, so the masking is not doing anything anywhere")
	}
}
