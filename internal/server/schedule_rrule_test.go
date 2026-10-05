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

// quarterClose is the rule cron cannot say: the last Friday of each quarter, at five in New York.
const quarterClose = "DTSTART;TZID=America/New_York:20260102T170000\n" +
	"RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR"

// TestScheduleAPIAcceptsRecurrence covers a schedule written with an RFC 5545 recurrence through
// the API: it is stored with its rule, takes its zone from the DTSTART, gets a first fire time from
// the rule, and an edit can move it between the two cadence forms.
func TestScheduleAPIAcceptsRecurrence(t *testing.T) {
	t.Parallel()
	schedules := schedule.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithSchedules(schedules)).Handler()
	send := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(string(raw))))
		return rec
	}

	rec := send(http.MethodPost, "/v1/schedules", map[string]any{
		"name": "quarter close", "rrule": quarterClose, "playbook": "close.yml",
	})
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
	if stored.RRule != quarterClose || stored.Cron != "" || stored.Timezone != "America/New_York" {
		t.Errorf("stored rrule=%q cron=%q timezone=%q, want the rule, no cron, and its zone",
			stored.RRule, stored.Cron, stored.Timezone)
	}
	if stored.NextRunAt == nil {
		t.Fatal("NextRunAt not set from the rule")
	}
	if local := stored.NextRunAt.In(mustLoad(t, "America/New_York")); local.Weekday() != time.Friday ||
		local.Hour() != 17 || local.AddDate(0, 0, 7).Month() == local.Month() {
		t.Errorf("first fire %v is not a last Friday at 17:00 New York", local)
	}

	tests := []struct {
		Body     map[string]any
		WantCode int
		WantText string
	}{{ // Test 0: A cron and a rule together are refused.
		Body:     map[string]any{"cron": "0 2 * * *", "rrule": quarterClose, "playbook": "p.yml"},
		WantCode: http.StatusBadRequest, WantText: "not both",
	}, { // Test 1: A bad rule names the part at fault.
		Body: map[string]any{"rrule": "DTSTART:20260102T170000Z RRULE:FREQ=FORTNIGHTLY",
			"playbook": "p.yml"},
		WantCode: http.StatusBadRequest, WantText: "FORTNIGHTLY",
	}, { // Test 2: A rule that already ran out is refused rather than stored never to fire.
		Body: map[string]any{"rrule": "DTSTART:20200102T170000Z RRULE:FREQ=DAILY;COUNT=2",
			"playbook": "p.yml"},
		WantCode: http.StatusBadRequest, WantText: "no fire after now",
	}, { // Test 3: A rule's zone that disagrees with the schedule's zone is refused.
		Body: map[string]any{"rrule": quarterClose, "timezone": "Europe/Paris",
			"playbook": "p.yml"},
		WantCode: http.StatusBadRequest, WantText: "make them agree",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := send(http.MethodPost, "/v1/schedules", test.Body)
			if rec.Code != test.WantCode || !strings.Contains(rec.Body.String(), test.WantText) {
				t.Errorf("create = %d %s, want %d naming %q", rec.Code, rec.Body.String(),
					test.WantCode, test.WantText)
			}
		})
	}

	// An edit back to cron clears the rule, and the zone it inherited stays.
	rec = send(http.MethodPut, "/v1/schedules/"+created.ID, map[string]any{
		"name": "quarter close", "cron": "0 17 * * 5", "playbook": "close.yml",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit to cron = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	stored, err = schedules.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.RRule != "" || stored.Cron != "0 17 * * 5" || stored.Timezone != "America/New_York" {
		t.Errorf("after edit rrule=%q cron=%q timezone=%q, want cron only in New York",
			stored.RRule, stored.Cron, stored.Timezone)
	}
}

// mustLoad loads a zone or fails the test.
func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q) error = %v", name, err)
	}
	return loc
}

// TestSchedulePreviewRecurrence covers the preview a form calls before saving: a recurrence
// previews its next five fires, and a bounded one previews what it has left and says it stops.
func TestSchedulePreviewRecurrence(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithSchedules(schedule.NewMemStore())).Handler()
	soon := time.Now().UTC().Add(time.Hour).Truncate(time.Minute).Format("20060102T150405Z")
	tests := []struct {
		Query        url.Values
		WantCode     int
		WantCount    int
		WantFinished bool
	}{{ // Test 0: An unbounded rule previews five fires.
		Query: url.Values{"rrule": {quarterClose}}, WantCode: http.StatusOK, WantCount: 5,
	}, { // Test 1: A rule with two fires left previews two and says it finishes.
		Query:    url.Values{"rrule": {"DTSTART:" + soon + " RRULE:FREQ=DAILY;COUNT=2"}},
		WantCode: http.StatusOK, WantCount: 2, WantFinished: true,
	}, { // Test 2: Neither form is a bad request.
		Query: url.Values{}, WantCode: http.StatusBadRequest,
	}, { // Test 3: A cron still previews as before.
		Query: url.Values{"cron": {"0 2 * * *"}}, WantCode: http.StatusOK, WantCount: 5,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/v1/schedules/preview?"+test.Query.Encode(), nil))
			if rec.Code != test.WantCode {
				t.Fatalf("preview = %d, want %d (body %s)", rec.Code, test.WantCode,
					rec.Body.String())
			}
			if test.WantCode != http.StatusOK {
				return
			}
			var body struct {
				Next     []time.Time `json:"next"`
				Finished bool        `json:"finished"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := []any{len(body.Next), body.Finished}
			if diff := cmp.Diff([]any{test.WantCount, test.WantFinished}, got); diff != "" {
				t.Errorf("preview mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
