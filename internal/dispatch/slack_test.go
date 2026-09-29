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

func TestSlackMessage(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	tests := []struct {
		In   *run.Run
		Want string
	}{{ // Test 0: A succeeded run uses the check icon and reports the elapsed time.
		In: &run.Run{
			Playbook: "deploy.yml", Status: run.StatusSucceeded, StartedAt: &start, EndedAt: &end,
		},
		Want: ":white_check_mark: SwitchTender run *deploy.yml* succeeded in 1m30s",
	}, { // Test 1: A failed run uses the cross icon and appends the error.
		In: &run.Run{
			Playbook: "deploy.yml", Status: run.StatusFailed, Error: "exit status 2",
			StartedAt: &start, EndedAt: &end,
		},
		Want: ":x: SwitchTender run *deploy.yml* failed in 1m30s\n> exit status 2",
	}, { // Test 2: A run without timing omits the elapsed clause, and a run with no playbook is named by
		// its tool and id rather than by its command, which is a script body that must not leave the host.
		In: &run.Run{
			ID: "r-2", Command: "terraform apply", Tool: "terraform", Status: run.StatusSucceeded,
		},
		Want: ":white_check_mark: SwitchTender run *terraform r-2* succeeded",
	}, { // Test 3: A run with no label falls back to the id.
		In:   &run.Run{ID: "r-1", Status: run.StatusSucceeded},
		Want: ":white_check_mark: SwitchTender run *r-1* succeeded",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := slackMessage(test.In)
			if diff := cmp.Diff(test.Want, got); diff != "" {
				t.Errorf("slackMessage mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDispatcherNotifiesSlack(t *testing.T) {
	t.Parallel()
	received := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
			received <- p.Text
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithSlack([]string{hook.URL}))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case got := <-received:
		if !strings.Contains(got, "play.yml") || !strings.Contains(got, "succeeded") {
			t.Errorf("slack message = %q, want it to mention play.yml and succeeded", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slack webhook never received the notification")
	}
}
