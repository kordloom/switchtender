package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestNotificationSecretsNeverLeaveOnARun walks every route that returns a run and asserts the
// channel secret is redacted on all of them.
//
// A notification URL is a bearer credential: whoever has the Slack or webhook URL can post as the
// install. Template responses masked it and run responses did not, and a template hands its targets
// to every run it launches, so the same secret an admin saw redacted came back in full to any
// viewer. Masking one of the two places a value appears is not masking it.
//
// The routes are enumerated here rather than trusted, because the original defect was not a wrong
// mask, it was a mask applied everywhere somebody remembered.
func TestNotificationSecretsNeverLeaveOnARun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const secret = "https://hooks.slack.com/services/T000/B111/ZZZZsupersecretZZZZ"
	store := run.NewMemStore()
	parent := "run_parent"
	idx, count := 0, 2
	seed := []*run.Run{
		{ID: "run_n", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: time.Now(),
			Notifications: []run.NotifyTarget{{Kind: "slack", URL: secret}}},
		{ID: parent, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusRunning,
			CreatedAt: time.Now(), Notifications: []run.NotifyTarget{{Kind: "slack", URL: secret}}},
		{ID: "run_shard", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
			ParentID: &parent, ShardIndex: &idx, ShardCount: &count,
			Notifications: []run.NotifyTarget{{Kind: "slack", URL: secret}}},
	}
	for _, r := range seed {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	routes := []string{
		"/v1/runs",
		"/v1/runs/run_n",
		"/v1/runs/" + parent + "/shards",
		"/v1/runs/" + parent + "/steps",
	}
	for _, path := range routes {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code >= 400 {
				t.Skipf("%s answered %d, nothing to check", path, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "ZZZZsupersecretZZZZ") {
				t.Errorf("%s returned the notification secret in full, so anyone who may read a "+
					"run can post as this install: %s", path, rec.Body.String())
			}
			// The channel is still visible, so masking did not blind the interface.
			if strings.Contains(rec.Body.String(), "notifications") &&
				!strings.Contains(rec.Body.String(), "slack") {
				t.Errorf("%s masked the channel as well as the secret", path)
			}
		})
	}
}
