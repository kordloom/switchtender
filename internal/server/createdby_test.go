package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// TestScheduleAndTriggerRecordWhoCreatedThem covers what an offboarding review needs.
//
// Neither carried any link to a person. A schedule kept launching real playbooks with real
// credentials after the account that set it up was deleted, and a trigger's token is a bearer
// credential belonging to the trigger rather than to anybody, so revoking the person revoked
// nothing. Neither of those firing behaviors changes here, deliberately: schedules are organization
// infrastructure and stopping production automation the moment somebody leaves is its own outage.
// What was missing is the record needed to decide, and that is what this adds.
func TestScheduleAndTriggerRecordWhoCreatedThem(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), actorKey{},
		Actor{UserID: "user_1", Name: "jane", Role: user.RoleAdmin})

	t.Run("schedule", func(t *testing.T) {
		t.Parallel()
		store := schedule.NewMemStore()
		handler := createScheduleHandler(store, nil, zap.NewNop())

		body := `{"name":"nightly","cron":"0 2 * * *","playbook":"site.yml","inventory":"hosts"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/schedules", strings.NewReader(body)).WithContext(ctx)
		handler(rec, req)
		if rec.Code != 201 {
			t.Fatalf("create returned %d: %s", rec.Code, rec.Body.String())
		}

		var got schedule.Schedule
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.CreatedBy != "jane" {
			t.Errorf("created_by = %q, want the actor who made it", got.CreatedBy)
		}

		// An edit does not rewrite it: the field records who set it up, not who last touched it.
		stored, err := store.Get(context.Background(), got.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.CreatedBy != "jane" {
			t.Errorf("stored created_by = %q, want it persisted", stored.CreatedBy)
		}
	})

	t.Run("trigger", func(t *testing.T) {
		t.Parallel()
		triggers := trigger.NewMemStore()
		templates := template.NewMemStore()
		if err := templates.Save(context.Background(),
			&template.Template{ID: "tpl_1", Name: "deploy", Playbook: "site.yml"}); err != nil {
			t.Fatalf("Save template: %v", err)
		}
		handler := createTriggerHandler(triggers, templates, nil, nil, zap.NewNop())

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/triggers",
			strings.NewReader(`{"name":"deploy-hook","template_id":"tpl_1"}`)).WithContext(ctx)
		handler(rec, req)
		if rec.Code != 201 {
			t.Fatalf("create returned %d: %s", rec.Code, rec.Body.String())
		}

		list, err := triggers.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("triggers = %d, want 1", len(list))
		}
		if list[0].CreatedBy != "jane" {
			t.Errorf("created_by = %q, want the actor who made it", list[0].CreatedBy)
		}
	})
}
