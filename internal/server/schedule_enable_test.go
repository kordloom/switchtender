package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// TestAScheduleCanBePausedAndResumed covers a field the product honors and no request could set.
//
// The tick loop skips a disabled schedule, so the flag works. Nothing could change it: create hardcoded
// it on, update copied whatever was stored, the request body declared no such field, and the decode is
// strict so sending one was refused. A live nightly deployment could therefore be stopped only by
// deleting it, which discards its cadence, its timezone, and its last-run history and cannot be undone
// by recreating it without them, and that is the operation an operator reaches for during an incident.
//
// It runs the other way too. An AWX import maps a schedule that was exported disabled faithfully, so
// every one of those arrived off with no way to ever turn it on: the migrated job silently never fired,
// and an operator who edited it got their edit and a schedule that still never fired.
func TestAScheduleCanBePausedAndResumed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := func(t *testing.T, enabled bool) (http.Handler, schedule.Store) {
		t.Helper()
		schedules := schedule.NewMemStore()
		if err := schedules.Save(ctx, &schedule.Schedule{
			ID: "sch_1", Name: "nightly deploy", Cron: "0 3 * * *", Timezone: "America/Chicago",
			Playbook: "site.yml", Inventory: "prod", Enabled: enabled, LastRunID: "run_last",
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		h := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
			WithSchedules(schedules)).Handler()
		return h, schedules
	}

	put := func(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/v1/schedules/sch_1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	const full = `{"name":"nightly deploy","cron":"0 3 * * *","timezone":"America/Chicago",` +
		`"playbook":"site.yml","inventory":"prod"`

	// Pausing a live schedule keeps everything else about it.
	t.Run("pause", func(t *testing.T) {
		t.Parallel()
		h, schedules := fixture(t, true)
		if rec := put(t, h, full+`,"enabled":false}`); rec.Code != http.StatusOK {
			t.Fatalf("pausing = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		got, err := schedules.Get(ctx, "sch_1")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Enabled {
			t.Error("the schedule is still enabled, so there is no way to pause a nightly deployment " +
				"during an incident except deleting it")
		}
		if got.Cron != "0 3 * * *" || got.Timezone != "America/Chicago" || got.LastRunID != "run_last" {
			t.Errorf("pausing changed the schedule: %+v", got)
		}
	})

	// Resuming a disabled one, which is what every AWX import needs.
	t.Run("resume", func(t *testing.T) {
		t.Parallel()
		h, schedules := fixture(t, false)
		if rec := put(t, h, full+`,"enabled":true}`); rec.Code != http.StatusOK {
			t.Fatalf("resuming = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		got, err := schedules.Get(ctx, "sch_1")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if !got.Enabled {
			t.Error("the schedule is still disabled, so an imported job can never be started")
		}
	})

	// An update that says nothing about the flag leaves it alone, so an edit dialog that cannot express
	// it cannot silently pause or resume a schedule.
	t.Run("an omitted flag is untouched", func(t *testing.T) {
		t.Parallel()
		for _, enabled := range []bool{true, false} {
			h, schedules := fixture(t, enabled)
			if rec := put(t, h, full+`}`); rec.Code != http.StatusOK {
				t.Fatalf("update = %d (body %s)", rec.Code, rec.Body.String())
			}
			got, err := schedules.Get(ctx, "sch_1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Enabled != enabled {
				t.Errorf("an update omitting the flag changed enabled from %v to %v", enabled, got.Enabled)
			}
		}
	})

	// A new schedule is live unless it says otherwise, which is what creating one means, and one created
	// paused stays paused.
	t.Run("create", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			// Name says what the body asked for.
			Name string
			// Body is the create request.
			Body string
			// WantEnabled is the flag the stored schedule must carry.
			WantEnabled bool
		}{
			{"by default", `{"cron":"0 3 * * *","playbook":"site.yml","inventory":"prod"}`, true},
			{"paused", `{"cron":"0 3 * * *","playbook":"site.yml","inventory":"prod","enabled":false}`, false},
		} {
			schedules := schedule.NewMemStore()
			h := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
				WithSchedules(schedules)).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(test.Body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("creating %s = %d (body %s)", test.Name, rec.Code, rec.Body.String())
			}
			var created schedule.Schedule
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if created.Enabled != test.WantEnabled {
				t.Errorf("a schedule created %s has enabled=%v, want %v",
					test.Name, created.Enabled, test.WantEnabled)
			}
		}
	})
}
