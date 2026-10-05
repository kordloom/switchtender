package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// TestScheduleCreatePinsTheServersZone creates schedules that name no zone through the API.
//
// A schedule naming no zone was read in the zone of whichever server evaluated it, so the two
// servers of a highly available pair whose clocks sat in different zones fired one daily schedule
// twice in a day. A create pins the server's zone by name, so the server that answered and every
// other read the schedule alike, and leaves a schedule that names its zone where it does.
func TestScheduleCreatePinsTheServersZone(t *testing.T) {
	t.Parallel()
	schedules := schedule.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithSchedules(schedules)).Handler()
	tests := []struct {
		// Body is the create request beside a name and a playbook.
		Body map[string]any
		// WantZone is the zone stored on the schedule.
		WantZone string
		// WantFireZone is the zone the first fire must land at 09:00 in.
		WantFireZone string
	}{{ // Test 0: A cron expression with no zone takes the server's.
		Body:     map[string]any{"cron": "0 9 * * *"},
		WantZone: schedule.ServerZone(), WantFireZone: schedule.ServerZone(),
	}, { // Test 1: A recurrence with a floating DTSTART takes the server's.
		Body:     map[string]any{"rrule": "DTSTART:20260101T090000 RRULE:FREQ=DAILY"},
		WantZone: schedule.ServerZone(), WantFireZone: schedule.ServerZone(),
	}, { // Test 2: A recurrence whose DTSTART names a zone takes that one.
		Body:     map[string]any{"rrule": "DTSTART;TZID=Europe/Berlin:20260101T090000 RRULE:FREQ=DAILY"},
		WantZone: "Europe/Berlin", WantFireZone: "Europe/Berlin",
	}, { // Test 3: A cron expression carrying its own descriptor is read in it and pins nothing.
		Body:     map[string]any{"cron": "CRON_TZ=Asia/Tokyo 0 9 * * *"},
		WantZone: "", WantFireZone: "Asia/Tokyo",
	}, { // Test 4: A named timezone is kept.
		Body:     map[string]any{"cron": "0 9 * * *", "timezone": "America/New_York"},
		WantZone: "America/New_York", WantFireZone: "America/New_York",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			body := map[string]any{"name": fmt.Sprintf("s%d", testNum), "playbook": "site.yml"}
			for k, v := range test.Body {
				body[k] = v
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/schedules",
				strings.NewReader(string(raw))))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
			}
			var created schedule.Schedule
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode: %v", err)
			}
			stored, err := schedules.Get(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(test.WantZone, stored.Timezone); diff != "" {
				t.Errorf("stored zone mismatch (-want +got):\n%s", diff)
			}
			// The first fire is worked out in the zone the schedule is read in.
			loc, err := time.LoadLocation(test.WantFireZone)
			if err != nil {
				t.Fatalf("load %s: %v", test.WantFireZone, err)
			}
			if stored.NextRunAt == nil || stored.NextRunAt.In(loc).Format("15:04") != "09:00" {
				t.Errorf("next fire = %v, want 09:00 in %s", stored.NextRunAt, test.WantFireZone)
			}
		})
	}
}

// TestScheduleUpdateOfAnUnnamedZoneKeepsWhereItFires edits a schedule an earlier release stored
// with no zone, with a body that names none either. The row is read in schedule.UnnamedZone, so the
// edit writes that zone onto it rather than leaving each server to read it in its own.
func TestScheduleUpdateOfAnUnnamedZoneKeepsWhereItFires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	schedules := schedule.NewMemStore()
	next := time.Now().Add(time.Hour)
	if err := schedules.Save(ctx, &schedule.Schedule{
		ID: "sch_old", Name: "old", Cron: "0 9 * * *", Playbook: "site.yml", Enabled: true,
		CreatedAt: time.Now().Add(-time.Hour), NextRunAt: &next,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithSchedules(schedules)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/schedules/sch_old",
		strings.NewReader(`{"name":"old","cron":"0 10 * * *","playbook":"site.yml"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err := schedules.Get(ctx, "sch_old")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(schedule.UnnamedZone, got.Timezone); diff != "" {
		t.Errorf("zone after the edit mismatch (-want +got):\n%s", diff)
	}
}

// TestSchedulePreviewPinsTheServersZone previews a cadence that names no zone. The preview reads it
// in the zone a create would pin and names that zone, so the times it shows are the times the
// schedule fires once saved.
func TestSchedulePreviewPinsTheServersZone(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithSchedules(schedule.NewMemStore())).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/schedules/preview?cron="+url.QueryEscape("0 9 * * *"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Next     []time.Time `json:"next"`
		Timezone string      `json:"timezone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if diff := cmp.Diff(schedule.ServerZone(), body.Timezone); diff != "" {
		t.Errorf("preview zone mismatch (-want +got):\n%s", diff)
	}
	loc, err := time.LoadLocation(schedule.ServerZone())
	if err != nil {
		t.Fatalf("load the server's zone: %v", err)
	}
	if len(body.Next) == 0 {
		t.Fatal("the preview returned no fires")
	}
	for _, at := range body.Next {
		if at.In(loc).Format("15:04") != "09:00" {
			t.Errorf("previewed fire %s is not 09:00 in %s", at, schedule.ServerZone())
		}
	}
}
