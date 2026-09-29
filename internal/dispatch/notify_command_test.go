package dispatch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// commandSecret is the inline credential in the run's script body. A shell run's command is a script,
// and people put passwords in scripts, which is why the product's rule is that a command never leaves
// the host.
const commandSecret = "Pr0dSecret-MUST-NOT-LEAVE"

// commandRun is a finished shell run whose command carries an inline credential, the shape every one of
// these channels formats.
func commandRun() *run.Run {
	started := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	return &run.Run{
		ID: "run_shell", Tool: run.ToolBash,
		Command:   "mysql -h db -u root --password=" + commandSecret + " -e 'drop table sessions'",
		Status:    run.StatusSucceeded,
		CreatedAt: started, StartedAt: &started, EndedAt: &ended,
		Actor: "casey", ActorType: "session",
	}
}

// TestNoChannelCarriesTheRunsCommandOffTheHost covers a leak in every external notification channel at
// once.
//
// The product states the rule in its own code: a command holds the raw script body of a bash, python,
// powershell, or go run, which can embed inline secrets or sensitive arguments, so the run store, the
// live stream, and the run page keep it and only what leaves the host loses it. One helper broke that
// everywhere. Every channel titles its message with a label that is the run's playbook, or its command
// when it has none, and a shell run has none, so the full script body went to Slack, Discord, Teams,
// PagerDuty, ntfy, Grafana, and Twilio. The Slack formatter's own comment claimed it exposed no channel
// secrets while the label was reintroducing the whole command.
func TestNoChannelCarriesTheRunsCommandOffTheHost(t *testing.T) {
	t.Parallel()
	r := commandRun()

	// Every formatter that composes a message for an external channel, rendered the way its channel
	// sends it.
	for _, test := range []struct {
		// Name is the channel.
		Name string
		// Render produces exactly what would be sent.
		Render func(*run.Run) string
	}{
		{"slack", func(r *run.Run) string { return slackMessage(r) }},
		{"discord", func(r *run.Run) string { return discordMessage(r) }},
		{"teams", func(r *run.Run) string { return mustJSON(t, teamsCardPayload(r)) }},
		{"grafana", func(r *run.Run) string { return grafanaText(r) }},
		{"label", func(r *run.Run) string { return runLabel(r) }},
	} {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got := test.Render(r)
			if strings.Contains(got, commandSecret) {
				t.Errorf("the %s message carries the run's command off the host:\n%s", test.Name, got)
			}
			// The message still has to identify the run, or the channel is useless.
			if !strings.Contains(got, r.ID) {
				t.Errorf("the %s message does not name the run, so nobody can find it:\n%s",
					test.Name, got)
			}
		})
	}

	// A playbook run is named by its playbook, which is a path rather than a script and is what an
	// operator recognizes, so that must not change.
	t.Run("a playbook is still named", func(t *testing.T) {
		t.Parallel()
		play := &run.Run{ID: "run_play", Playbook: "site.yml", Status: run.StatusSucceeded}
		if !strings.Contains(runLabel(play), "site.yml") {
			t.Errorf("runLabel = %q, want the playbook named", runLabel(play))
		}
	})
}

// TestAPerRunWebhookRedactsWhatTheServerWideOneDoes covers the two webhook paths disagreeing.
//
// A run can carry its own notification targets, and a template that sets one sends the same JSON shape
// to the same class of third-party endpoint as the server-wide webhook. The per-run path listed the
// fields it cleared by hand and its comment claimed it redacted exactly as the server-wide channel
// does, which was not true: it kept the command. So whether a third party received the run's script
// body, inline credentials and all, depended on which of two webhooks an operator had configured.
func TestAPerRunWebhookRedactsWhatTheServerWideOneDoes(t *testing.T) {
	t.Parallel()
	r := commandRun()
	r.Notifications = []run.NotifyTarget{{
		Kind: run.NotifyWebhook, URL: "https://intake.example/hook", Key: "routing-key-secret",
	}}
	r.ExtraVars = map[string]any{"db_password": "extra-var-secret"}

	// The real path: the dispatcher delivers to the target's URL, and the body it posts is what a third
	// party receives. Rendering the redaction helper directly would pass while the branch that calls it
	// still assembled its own.
	posted := make(chan []byte, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		got, _ := io.ReadAll(req.Body)
		select {
		case posted <- got:
		default:
		}
	}))
	defer sink.Close()
	r.Notifications[0].URL = sink.URL

	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithNoJanitor())
	defer d.Close()
	d.notifyRunTargets(r)
	d.notifyWG.Wait()

	var body string
	select {
	case got := <-posted:
		body = string(got)
	case <-time.After(10 * time.Second):
		t.Fatal("the per-run webhook posted nothing")
	}

	for _, secret := range []struct {
		// What names the field, for the failure message.
		What string
		// Value must not appear in the body.
		Value string
	}{
		{"command", commandSecret},
		{"notification key", "routing-key-secret"},
		{"extra var", "extra-var-secret"},
	} {
		if strings.Contains(body, secret.Value) {
			t.Errorf("the webhook body carries the run's %s:\n%s", secret.What, body)
		}
	}
	// It still says which run finished and how, or the webhook conveys nothing.
	if !strings.Contains(body, r.ID) || !strings.Contains(body, string(run.StatusSucceeded)) {
		t.Errorf("the webhook body does not report the run and its status:\n%s", body)
	}
}

// mustJSON renders v the way a channel posts it.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(body)
}
