package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// grafanaToken is a token distinctive enough to prove it never reaches a response.
const grafanaToken = "glsa_NTF_GRAFANA_TOKEN"

// TestFinishingATargetAsksOnlyForWhatIsMissing pins decision 38 at the API: a target an import left
// waiting for its secret keeps what it has, reads back as needing exactly the missing part, takes
// an edit that leaves it still waiting, and is finished by sending the missing secret alone, after
// which its other parts are the ones the import kept; the secret is never sent back.
func TestFinishingATargetAsksOnlyForWhatIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newNotifyFixture(t, true)
	shell := &notification.Notification{ID: "ntf_grafana", Name: "dashboards",
		Description: "from AWX", CreatedAt: time.Now()}
	if err := shell.SetKnown(run.NotifyTarget{Kind: run.NotifyGrafana,
		URL: "https://grafana.example.com"}, f.sealer); err != nil {
		t.Fatalf("SetKnown() error = %v", err)
	}
	if err := f.store.Save(ctx, shell); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	read := func() notificationView {
		t.Helper()
		rec := f.do(t, http.MethodGet, "/v1/notifications/ntf_grafana", f.admin, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), grafanaToken) {
			t.Errorf("a read carries the token: %s", rec.Body.String())
		}
		var v struct {
			notification.Notification
			// Delivery is the delivery status.
			Delivery notification.Health `json:"delivery"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return notificationView{Notification: &v.Notification, Delivery: v.Delivery}
	}

	tests := []struct {
		Body        map[string]any
		WantState   string
		WantMissing []string
		WantName    string
	}{{ // Test 0: As imported, it needs the token and nothing else.
		WantState: notification.StateNeedsSecret, WantMissing: []string{"key"},
		WantName: "dashboards",
	}, { // Test 1: A rename leaves it waiting, and keeps the address it has.
		Body:      map[string]any{"name": "grafana dashboards"},
		WantState: notification.StateNeedsSecret, WantMissing: []string{"key"},
		WantName: "grafana dashboards",
	}, { // Test 2: The token alone finishes it.
		Body:      map[string]any{"name": "grafana dashboards", "key": grafanaToken},
		WantState: notification.StateConfigured, WantName: "grafana dashboards",
	}, { // Test 3: An edit that sends no secret keeps the stored one.
		Body:      map[string]any{"name": "dashboards", "key": "", "url": ""},
		WantState: notification.StateConfigured, WantName: "dashboards",
	}}
	for testNum, test := range tests {
		if test.Body != nil {
			rec := f.do(t, http.MethodPut, "/v1/notifications/ntf_grafana", f.admin, test.Body)
			if rec.Code != http.StatusOK {
				t.Fatalf("test %d: PUT = %d: %s", testNum, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), grafanaToken) {
				t.Errorf("test %d: the response carries the token", testNum)
			}
		}
		v := read()
		got := []any{v.Delivery.State, v.Delivery.Missing, v.Name}
		want := []any{test.WantState, test.WantMissing, test.WantName}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d: read back mismatch (-want +got):\n%s", testNum, diff)
		}
	}
	stored, err := f.store.Get(ctx, "ntf_grafana")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	opened, err := stored.Target(f.sealer)
	want := run.NotifyTarget{Kind: run.NotifyGrafana, URL: "https://grafana.example.com",
		Key: grafanaToken}
	if err != nil {
		t.Fatalf("Target() error = %v", err)
	}
	if diff := cmp.Diff(want, opened); diff != "" {
		t.Errorf("the finished target mismatch (-want +got):\n%s", diff)
	}
	if stored.Description != "from AWX" {
		t.Errorf("the description the import kept was lost: %q", stored.Description)
	}
}

// TestDeliveryStatusIsReadable pins the target's delivery status and the two delivery listings: a
// failed delivery is kept and shown on the target, on the run, and to the target's listing, and a
// caller without the operator role reads none of them.
func TestDeliveryStatusIsReadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newNotifyFixture(t, true)
	id := f.create(t, map[string]any{"name": "ops slack", "kind": "slack", "url": slackSecret})
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for seq, event := range []string{"started", "failure"} {
		ok, err := f.store.Record(ctx, &notification.RunEvent{RunID: "run_seen", Event: event,
			Snapshot:  []byte(fmt.Sprintf(`{"id":"run_seen","n":%d}`, seq)),
			CreatedAt: at.Add(time.Duration(seq) * time.Second)}, []notification.Recipient{
			{NotificationID: id, Name: "ops slack", Kind: "slack"}})
		if err != nil || !ok {
			t.Fatalf("Record() = %v, %v", ok, err)
		}
	}
	claimed, err := f.store.Claim(ctx, "w", at.Add(time.Minute), time.Minute, 10, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %v, %v", claimed, err)
	}
	if err := f.store.Finish(ctx, claimed[0], "w", notification.Outcome{
		Status: notification.DeliveryFailed, Error: "the target answered 404",
		At: at.Add(time.Minute)}); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	rec := f.do(t, http.MethodGet, "/v1/notifications", f.admin, nil)
	var list notificationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Notifications) != 1 {
		t.Fatalf("GET /v1/notifications = %d %s", rec.Code, rec.Body.String())
	}
	h := list.Notifications[0].Delivery
	if h.State != notification.StateFailing || h.Failed != 1 || h.Pending != 1 ||
		h.LastError != "the target answered 404" {
		t.Errorf("delivery status = %+v, want failing with one failure and one pending", h)
	}
	rec = f.do(t, http.MethodGet, "/v1/notifications/"+id+"/deliveries?status=failed", f.admin, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"count":1`) ||
		!strings.Contains(rec.Body.String(), "the target answered 404") {
		t.Errorf("the target's failed deliveries = %d %s", rec.Code, rec.Body.String())
	}
	if err := f.runs.Save(ctx, &run.Run{ID: "run_seen", Status: run.StatusFailed,
		CreatedAt: at}); err != nil {
		t.Fatalf("Save(run) error = %v", err)
	}
	rec = f.do(t, http.MethodGet, "/v1/runs/run_seen/notifications", f.admin, nil)
	var onRun deliveriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &onRun); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET the run's notifications = %d %s", rec.Code, rec.Body.String())
	}
	var order []string
	for _, d := range onRun.Deliveries {
		order = append(order, fmt.Sprintf("%d %s %s", d.Seq, d.Event, d.Status))
	}
	if diff := cmp.Diff([]string{"1 started failed", "2 failure pending"}, order); diff != "" {
		t.Errorf("the run's notifications mismatch (-want +got):\n%s", diff)
	}
	for _, path := range []string{"/v1/notifications/" + id + "/deliveries",
		"/v1/runs/run_seen/notifications"} {
		rec := f.do(t, http.MethodGet, path, "not-a-token", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, rec.Code)
		}
	}
}
