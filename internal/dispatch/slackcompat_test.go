package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDispatcherNotifiesSlackCompat verifies the Mattermost and Rocket.Chat channels each receive the
// Slack-compatible text payload when a run finishes.
func TestDispatcherNotifiesSlackCompat(t *testing.T) {
	t.Parallel()
	capture := func(ch chan string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
				ch <- p.Text
			}
			w.WriteHeader(http.StatusOK)
		}
	}
	mm := make(chan string, 4)
	rc := make(chan string, 4)
	mmSrv := httptest.NewServer(capture(mm))
	defer mmSrv.Close()
	rcSrv := httptest.NewServer(capture(rc))
	defer rcSrv.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil,
		WithNotifyClient(http.DefaultClient),
		WithMattermost([]string{mmSrv.URL}), WithRocketChat([]string{rcSrv.URL}))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	for name, ch := range map[string]chan string{"mattermost": mm, "rocketchat": rc} {
		select {
		case got := <-ch:
			if !strings.Contains(got, "play.yml") || !strings.Contains(got, "succeeded") {
				t.Errorf("%s message = %q, want it to mention play.yml and succeeded", name, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s webhook never received the notification", name)
		}
	}
}
