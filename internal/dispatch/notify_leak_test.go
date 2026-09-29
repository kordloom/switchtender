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

// leakTargets are the per-run notification targets whose secrets must never reach an external
// destination: a PagerDuty routing key and a webhook URL are real bearer secrets.
func leakTargets() []run.NotifyTarget {
	return []run.NotifyTarget{
		{Kind: run.NotifyPagerDuty, Key: leakRoutingKey},
		{Kind: run.NotifyWebhook, URL: leakWebhookURL},
	}
}

const (
	// leakRoutingKey is a PagerDuty routing key a target carries, a bearer secret.
	leakRoutingKey = "secret-rk"
	// leakWebhookURL is a per-run webhook URL a target carries, a bearer secret.
	leakWebhookURL = "https://hooks.example/secret-webhook-path"
)

// TestNotifyExtraRedactsNotificationTargets proves a run handed to a registered external Notifier
// carries no per-run notification targets, since each target holds a routing key or API token and the
// notifier marshals the whole run to an external plugin process. It does not call t.Parallel: it
// writes the package notifier registry, so it runs in the sequential phase.
func TestNotifyExtraRedactsNotificationTargets(t *testing.T) {
	got := make(chan *run.Run, 8)
	RegisterNotifier("regleak", NotifierFunc(func(_ context.Context, r *run.Run) error {
		select {
		case got <- r:
		default:
		}
		return nil
	}))

	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), nil)
	defer d.Close()

	d.notifyExtra(&run.Run{
		ID: "r-leak", Playbook: "p.yml", Status: run.StatusSucceeded,
		ExtraVars:     map[string]any{"answer": "x"},
		Notifications: leakTargets(),
	})

	select {
	case rec := <-got:
		if rec.Notifications != nil {
			t.Errorf("notifier received notification targets %+v, want them redacted", rec.Notifications)
		}
		if rec.ExtraVars != nil {
			t.Errorf("notifier received extra vars %v, want them redacted", rec.ExtraVars)
		}
		// The plugin adapter marshals the whole run to an external process, so assert on the bytes.
		body, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		assertNoLeak(t, body)
	case <-time.After(3 * time.Second):
		t.Fatal("registered notifier never received the run")
	}
}

// TestNotifyWebhooksRedactsNotificationTargets proves the JSON body posted to a server-wide webhook
// carries no per-run notification targets, so a routing key or webhook secret a run carries never
// reaches an external endpoint.
func TestNotifyWebhooksRedactsNotificationTargets(t *testing.T) {
	t.Parallel()
	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), nil, WithWebhooks([]string{srv.URL}))
	defer d.Close()

	d.notifyWebhooks(&run.Run{
		ID: "r-hook", Playbook: "p.yml", Status: run.StatusSucceeded,
		ExtraVars:     map[string]any{"answer": "x"},
		Notifications: leakTargets(),
	})

	select {
	case body := <-bodies:
		var got notification
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		if got.Run.Notifications != nil {
			t.Errorf("webhook body carried targets %+v, want them redacted", got.Run.Notifications)
		}
		if got.Run.ExtraVars != nil {
			t.Errorf("webhook body carried extra vars %v, want them redacted", got.Run.ExtraVars)
		}
		assertNoLeak(t, body)
	case <-time.After(3 * time.Second):
		t.Fatal("webhook never received the notification")
	}
}

// assertNoLeak fails if an external payload contains either target secret.
func assertNoLeak(t *testing.T, payload []byte) {
	t.Helper()
	s := string(payload)
	if strings.Contains(s, leakRoutingKey) {
		t.Errorf("external payload leaked the routing key: %s", s)
	}
	if strings.Contains(s, leakWebhookURL) {
		t.Errorf("external payload leaked the webhook secret: %s", s)
	}
}
