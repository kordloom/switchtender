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

	"github.com/kordloom/switchtender/internal/dispatchtest"
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
	d := New(store, runner, nil, WithSlack([]string{hook.URL}), WithNotifyClient(http.DefaultClient))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	dispatchtest.WaitTerminal(t, store, created.ID)

	select {
	case got := <-received:
		if !strings.Contains(got, "play.yml") || !strings.Contains(got, "succeeded") {
			t.Errorf("slack message = %q, want it to mention play.yml and succeeded", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slack webhook never received the notification")
	}
}

// hostileText is a value an agent could put in a playbook, a hold note, a policy name, an actor, or
// an error: a Slack link with its own text, emphasis, and a channel-wide mention.
const hostileText = "a<https://x.example/|b>*@everyone"

// hostileRuns returns a held run and a failed run carrying hostileText in every outside field a
// notice shows, so each channel's escaper is held to all of them.
func hostileRuns() []*run.Run {
	return []*run.Run{{
		ID: "r-held", Playbook: hostileText, Status: run.StatusPendingApproval,
		HoldNote: hostileText, HeldByPolicy: hostileText, Actor: hostileText,
	}, {
		ID: "r-failed", Playbook: hostileText, Status: run.StatusFailed, Error: hostileText,
	}, {
		ID: "r-step", Playbook: hostileText, Status: run.StatusPendingApproval, Actor: hostileText,
		AwaitingStep: &run.AwaitingStep{Name: hostileText, Description: hostileText,
			OnApprove: []string{hostileText}},
	}}
}

// TestSlackMessageEscapesOutsideText proves a playbook, hold note, policy name, actor, error, or
// step from outside cannot plant a Slack link or a mention, and that the run itself is untouched.
func TestSlackMessageEscapesOutsideText(t *testing.T) {
	t.Parallel()
	for testNum, in := range hostileRuns() {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := slackMessage(in)
			if !strings.Contains(got, "&lt;") || strings.Contains(got, "<https") {
				t.Errorf("slackMessage() = %q, want &lt; and no <https", got)
			}
			if strings.ContainsAny(strings.ReplaceAll(got, "\n> ", ""), "<>") {
				t.Errorf("slackMessage() = %q, want no bare angle bracket", got)
			}
			if in.Playbook != hostileText {
				t.Errorf("slackMessage() changed the run's playbook to %q", in.Playbook)
			}
		})
	}
}
