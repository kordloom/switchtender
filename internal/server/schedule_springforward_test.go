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

// TestScheduleAPISpringForward covers the spring-forward setting through the API: a create stores
// it, an edit that does not name it keeps it, an edit that names it changes it, an explicit empty
// string returns the schedule to its cadence's default, and an unknown value is refused with the
// choices.
func TestScheduleAPISpringForward(t *testing.T) {
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
	stored := func(id string) string {
		sc, err := schedules.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		return sc.SpringForward
	}

	rec := send(http.MethodPost, "/v1/schedules", map[string]any{
		"name": "nightly", "cron": "30 2 * * *", "timezone": "America/New_York",
		"playbook": "site.yml", "spring_forward": "skip",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created schedule.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.SpringForward != schedule.SpringForwardSkip || stored(created.ID) != "skip" {
		t.Fatalf("created spring_forward = %q, stored %q, want skip on both",
			created.SpringForward, stored(created.ID))
	}

	edit := map[string]any{"name": "nightly", "cron": "30 2 * * *", "playbook": "site.yml"}
	with := func(setting string) map[string]any {
		out := map[string]any{"spring_forward": setting}
		for k, v := range edit {
			out[k] = v
		}
		return out
	}
	tests := []struct {
		Body        map[string]any
		WantText    string
		WantSetting string
		WantCode    int
	}{{ // Test 0: An edit that does not name the setting keeps it.
		Body: edit, WantCode: http.StatusOK, WantSetting: "skip",
	}, { // Test 1: An edit that names a setting changes it.
		Body: with("later"), WantCode: http.StatusOK, WantSetting: "later",
	}, { // Test 2: An explicit empty string returns the schedule to its default.
		Body: with(""), WantCode: http.StatusOK, WantSetting: "",
	}, { // Test 3: An unknown setting is refused with the choices, and nothing changes.
		Body: with("sometimes"), WantCode: http.StatusBadRequest, WantSetting: "",
		WantText: "jump, later, or skip",
	}}
	for testNum, test := range tests {
		rec := send(http.MethodPut, "/v1/schedules/"+created.ID, test.Body)
		if rec.Code != test.WantCode || !strings.Contains(rec.Body.String(), test.WantText) {
			t.Errorf("test %d: edit = %d %s, want %d naming %q", testNum, rec.Code,
				rec.Body.String(), test.WantCode, test.WantText)
		}
		if got := stored(created.ID); got != test.WantSetting {
			t.Errorf("test %d: stored spring_forward = %q, want %q", testNum, got,
				test.WantSetting)
		}
	}

	rec = send(http.MethodPost, "/v1/schedules", map[string]any{
		"cron": "30 2 * * *", "playbook": "site.yml", "spring_forward": "whenever",
	})
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "jump, later, or skip") {
		t.Errorf("create with an unknown setting = %d %s, want 400 naming the choices", rec.Code,
			rec.Body.String())
	}
}

// nextSpringTransition returns the first instant after now that loc's clocks jump forward, found
// from the zone's own bounds rather than from any code under test.
func nextSpringTransition(t *testing.T, loc *time.Location, now time.Time) time.Time {
	t.Helper()
	at := now.In(loc)
	for range 8 {
		_, end := at.ZoneBounds()
		if end.IsZero() {
			break
		}
		_, before := at.Zone()
		_, after := end.Zone()
		if after > before {
			return end
		}
		at = end
	}
	t.Fatalf("%s has no spring-forward transition ahead of %v", loc, now)
	return time.Time{}
}

// TestSchedulePreviewSpringForward covers the preview with the setting: its fires are computed
// under it, a bad value is refused, and the night the clocks go forward over a time the schedule
// names is described with where it fires under the chosen setting, while a schedule the setting
// changes nothing for gets no such description.
func TestSchedulePreviewSpringForward(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithSchedules(schedule.NewMemStore())).Handler()
	ny := mustLoad(t, "America/New_York")
	jump := nextSpringTransition(t, ny, time.Now())
	nightly := url.Values{"cron": {"30 2 * * *"}, "timezone": {"America/New_York"}}
	withSetting := func(v url.Values, setting string) url.Values {
		out := url.Values{"spring_forward": {setting}}
		for k, vals := range v {
			out[k] = vals
		}
		return out
	}
	tests := []struct {
		Query       url.Values
		WantText    string
		WantSetting string
		WantFires   []time.Time
		WantCode    int
		WantGap     bool
	}{{ // Test 0: A cron with no setting describes the jump it fires at.
		Query: nightly, WantCode: http.StatusOK, WantGap: true, WantSetting: "jump",
		WantFires: []time.Time{jump},
	}, { // Test 1: Later fires half an hour after the jump.
		Query: withSetting(nightly, "later"), WantCode: http.StatusOK, WantGap: true,
		WantSetting: "later", WantFires: []time.Time{jump.Add(30 * time.Minute)},
	}, { // Test 2: Skip is described with nothing firing in that hour.
		Query: withSetting(nightly, "skip"), WantCode: http.StatusOK, WantGap: true,
		WantSetting: "skip",
	}, { // Test 3: A recurrence rule defaults to later, as AWX does.
		Query: url.Values{"rrule": {"DTSTART;TZID=America/New_York:20260101T023000 " +
			"RRULE:FREQ=DAILY"}},
		WantCode: http.StatusOK, WantGap: true, WantSetting: "later",
		WantFires: []time.Time{jump.Add(30 * time.Minute)},
	}, { // Test 4: An hourly cron fires the same under every setting and is not described.
		Query:    url.Values{"cron": {"0 * * * *"}, "timezone": {"America/New_York"}},
		WantCode: http.StatusOK,
	}, { // Test 5: An unknown setting is refused with the choices.
		Query: withSetting(nightly, "sometimes"), WantCode: http.StatusBadRequest,
		WantText: "jump, later, or skip",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/v1/schedules/preview?"+test.Query.Encode(), nil))
			if rec.Code != test.WantCode || !strings.Contains(rec.Body.String(), test.WantText) {
				t.Fatalf("preview = %d %s, want %d naming %q", rec.Code, rec.Body.String(),
					test.WantCode, test.WantText)
			}
			if test.WantCode != http.StatusOK {
				return
			}
			var body struct {
				// Next are the five fires.
				Next []time.Time `json:"next"`
				// Gap describes the night the clocks go forward.
				Gap *schedule.SpringGap `json:"spring_gap"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Next) != 5 {
				t.Errorf("preview listed %d fires, want 5", len(body.Next))
			}
			if !test.WantGap {
				if body.Gap != nil {
					t.Errorf("spring_gap = %+v, want none for a schedule no setting changes",
						body.Gap)
				}
				return
			}
			if body.Gap == nil {
				t.Fatalf("no spring_gap in %s", rec.Body.String())
			}
			second := func(ts []time.Time) []string {
				out := []string{}
				for _, v := range ts {
					out = append(out, v.UTC().Truncate(time.Second).Format(time.RFC3339))
				}
				return out
			}
			got := []any{body.Gap.Setting, body.Gap.Zone, body.Gap.From, body.Gap.To,
				body.Gap.Transition.UTC().Format(time.RFC3339), second(body.Gap.Fires)}
			want := []any{test.WantSetting, "America/New_York", "02:00", "03:00",
				jump.UTC().Format(time.RFC3339), second(test.WantFires)}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("spring_gap mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
