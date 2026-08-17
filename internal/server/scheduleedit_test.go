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

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// TestScheduleEditKeepsWhatItDoesNotSend covers editing a schedule the sender does not fully know
// about, which is every schedule an import produced and every one whose shape the edit dialog cannot
// express. The update handler rebuilds the schedule whole, so a field the sender leaves out is a
// field that gets erased: a pipeline schedule edited to change its cadence came back as a schedule
// that fires one playbook, and a split schedule lost its shards and began running everything at once
// on one worker. Both changes are silent and both change what executes.
func TestScheduleEditKeepsWhatItDoesNotSend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	schedules := schedule.NewMemStore()

	pipeline := &schedule.Schedule{
		ID: "sch_pipeline", Name: "nightly", Cron: "0 2 * * *", Timezone: "America/Chicago",
		Steps: []run.PipelineStep{
			{Name: "plan", Tool: "terraform", Command: "/srv/infra"},
			{Name: "apply", Tool: "terraform", Command: "/srv/infra", DependsOn: []string{"plan"}},
		},
		Enabled: true, CreatedAt: time.Now(),
	}
	if err := schedules.Save(ctx, pipeline); err != nil {
		t.Fatalf("Save schedule: %v", err)
	}
	split := &schedule.Schedule{
		ID: "sch_split", Name: "fleet patch", Cron: "0 3 * * *", Playbook: "patch.yml",
		Inventory: "prod.ini", Shards: 8, Enabled: true, CreatedAt: time.Now(),
	}
	if err := schedules.Save(ctx, split); err != nil {
		t.Fatalf("Save schedule: %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithSchedules(schedules)).Handler()
	put := func(id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/v1/schedules/"+id, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	// Test 0: A cadence change from a dialog that knows nothing about steps keeps the steps.
	rec := put("sch_pipeline", `{"name":"nightly","cron":"30 2 * * *","timezone":"America/Chicago"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit pipeline schedule = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err := schedules.Get(ctx, "sch_pipeline")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Steps) != 2 {
		t.Errorf("steps after a cadence edit = %d, want 2: the edit turned a pipeline into a "+
			"single run", len(got.Steps))
	}
	if got.Cron != "30 2 * * *" {
		t.Errorf("cron after edit = %q, want the new cadence", got.Cron)
	}

	// Test 1: The same for a split's shard count.
	rec = put("sch_split", `{"name":"fleet patch","cron":"0 4 * * *","playbook":"patch.yml",`+
		`"inventory":"prod.ini"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit split schedule = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err = schedules.Get(ctx, "sch_split")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Shards != 8 {
		t.Errorf("shards after a cadence edit = %d, want 8: the edit collapsed a fleet-wide split "+
			"onto one worker", got.Shards)
	}

	// Test 2: A sender that means to change the shape still can, by saying so explicitly.
	rec = put("sch_split", `{"name":"fleet patch","cron":"0 4 * * *","playbook":"patch.yml",`+
		`"inventory":"prod.ini","shards":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("flatten split = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err = schedules.Get(ctx, "sch_split")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Shards != 0 {
		t.Errorf("shards after an explicit flatten = %d, want 0", got.Shards)
	}

	// Test 3: And a pipeline can be replaced outright.
	rec = put("sch_pipeline", `{"name":"nightly","cron":"30 2 * * *","playbook":"site.yml",`+
		`"steps":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace pipeline = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err = schedules.Get(ctx, "sch_pipeline")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Steps) != 0 || got.Playbook != "site.yml" {
		var out []byte
		out, _ = json.Marshal(got)
		t.Errorf("schedule after an explicit replacement = %s, want one playbook and no steps", out)
	}
}
