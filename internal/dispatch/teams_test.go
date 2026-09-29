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

func TestTeamsCardPayload(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)

	// A succeeded run: an Adaptive Card with a Good-colored title and status and elapsed facts.
	ok, _ := json.Marshal(teamsCardPayload(&run.Run{
		Playbook: "deploy.yml", Status: run.StatusSucceeded, StartedAt: &start, EndedAt: &end,
	}))
	for _, want := range []string{
		`"type":"AdaptiveCard"`, "SwitchTender run deploy.yml",
		`"color":"Good"`, `"value":"succeeded"`, `"value":"1m30s"`,
		`"contentType":"application/vnd.microsoft.card.adaptive"`,
	} {
		if !strings.Contains(string(ok), want) {
			t.Errorf("card missing %q in %s", want, ok)
		}
	}

	// A failed run: an Attention-colored title and an error fact.
	bad, _ := json.Marshal(teamsCardPayload(&run.Run{
		Playbook: "deploy.yml", Status: run.StatusFailed, Error: "exit status 2",
		StartedAt: &start, EndedAt: &end,
	}))
	for _, want := range []string{`"color":"Attention"`, `"value":"exit status 2"`} {
		if !strings.Contains(string(bad), want) {
			t.Errorf("failed card missing %q in %s", want, bad)
		}
	}
}

func TestDispatcherNotifiesTeams(t *testing.T) {
	t.Parallel()
	received := make(chan teamsMessage, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m teamsMessage
		if err := json.NewDecoder(r.Body).Decode(&m); err == nil {
			received <- m
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithTeams([]string{hook.URL}))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case got := <-received:
		if len(got.Attachments) != 1 {
			t.Fatalf("attachments = %d, want 1", len(got.Attachments))
		}
		title, _ := got.Attachments[0].Content.Body[0]["text"].(string)
		if !strings.Contains(title, "play.yml") {
			t.Errorf("teams card title = %q, want it to mention play.yml", title)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("teams webhook never received the notification")
	}
}
