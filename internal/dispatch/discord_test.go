package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

func TestDiscordMessage(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	tests := []struct {
		In   *run.Run
		Want string
	}{{ // Test 0: A succeeded run uses the check emoji and reports the elapsed time.
		In: &run.Run{
			Playbook: "deploy.yml", Status: run.StatusSucceeded, StartedAt: &start, EndedAt: &end,
		},
		Want: "✅ SwitchTender run **deploy.yml** succeeded in 1m30s",
	}, { // Test 1: A failed run uses the cross emoji and appends the error.
		In: &run.Run{
			Playbook: "deploy.yml", Status: run.StatusFailed, Error: "exit status 2",
			StartedAt: &start, EndedAt: &end,
		},
		Want: "❌ SwitchTender run **deploy.yml** failed in 1m30s\n> exit status 2",
	}, { // Test 2: A run with no label falls back to the id.
		In:   &run.Run{ID: "r-1", Status: run.StatusSucceeded},
		Want: "✅ SwitchTender run **r-1** succeeded",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, discordMessage(test.In)); diff != "" {
				t.Errorf("discordMessage mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDispatcherNotifiesDiscord(t *testing.T) {
	t.Parallel()
	received := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
			received <- p.Content
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithDiscord([]string{hook.URL}))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case got := <-received:
		if !strings.Contains(got, "play.yml") || !strings.Contains(got, "succeeded") {
			t.Errorf("discord message = %q, want it to mention play.yml and succeeded", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("discord webhook never received the notification")
	}
}
