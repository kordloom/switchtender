package dispatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestPerTemplatePagerDutyUsesTargetKey proves a run carrying a PagerDuty target pages the target's
// own routing key, not any server-wide key, and only on a failed run.
func TestPerTemplatePagerDutyUsesTargetKey(t *testing.T) {
	t.Parallel()
	received := make(chan pagerDutyEvent, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e pagerDutyEvent
		_ = json.NewDecoder(r.Body).Decode(&e)
		received <- e
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 2}, nil
		}), nil)
	d.pagerDutyEndpoint = srv.URL
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications([]run.NotifyTarget{{Kind: run.NotifyPagerDuty, Key: "team-rk"}}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := waitTerminal(t, store, created.ID); got.Status != run.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	select {
	case e := <-received:
		if e.RoutingKey != "team-rk" || e.EventAction != "trigger" || e.DedupKey != created.ID {
			t.Errorf("event = %+v, want a trigger for team-rk deduped on the run id", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("per-template pagerduty target did not page")
	}
}

// TestPerTemplatePagerDutySilentOnSuccess proves a succeeded run pages no one, since a PagerDuty
// target is a failure signal and a page on every green run trains an operator to ignore it.
func TestPerTemplatePagerDutySilentOnSuccess(t *testing.T) {
	t.Parallel()
	received := make(chan pagerDutyEvent, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e pagerDutyEvent
		_ = json.NewDecoder(r.Body).Decode(&e)
		received <- e
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), nil)
	d.pagerDutyEndpoint = srv.URL
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications([]run.NotifyTarget{{Kind: run.NotifyPagerDuty, Key: "team-rk"}}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)
	select {
	case e := <-received:
		t.Errorf("a succeeded run paged %+v, want silence", e)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestPerTemplateGrafanaUsesTargetURLAndToken proves a Grafana target annotates the instance the
// template names, authenticated with the template's own token, not a server-wide Grafana.
func TestPerTemplateGrafanaUsesTargetURLAndToken(t *testing.T) {
	t.Parallel()
	type capture struct {
		path, auth string
		ann        grafanaAnnotation
	}
	received := make(chan capture, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a grafanaAnnotation
		_ = json.NewDecoder(r.Body).Decode(&a)
		received <- capture{path: r.URL.Path, auth: r.Header.Get("Authorization"), ann: a}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications([]run.NotifyTarget{{Kind: run.NotifyGrafana, URL: srv.URL, Key: "gf-team"}}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)
	select {
	case got := <-received:
		if got.path != "/api/annotations" {
			t.Errorf("path = %q, want /api/annotations", got.path)
		}
		if got.auth != "Bearer gf-team" {
			t.Errorf("auth = %q, want the template's token Bearer gf-team", got.auth)
		}
		if !strings.Contains(got.ann.Text, "play.yml") {
			t.Errorf("annotation text = %q, want it to mention the play", got.ann.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("per-template grafana target did not annotate")
	}
}

// TestPerTemplateTwilioTextsTargetRecipient proves a Twilio target texts the number the template
// names through the server-held Twilio account, since the account secret is not a per-run value.
func TestPerTemplateTwilioTextsTargetRecipient(t *testing.T) {
	t.Parallel()
	type capture struct {
		path, auth, to, from string
	}
	received := make(chan capture, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		received <- capture{
			path: r.URL.Path, auth: r.Header.Get("Authorization"),
			to: r.PostForm.Get("To"), from: r.PostForm.Get("From"),
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 2}, nil
		}), nil, WithTwilio("AC123", "tok", "+15550000", nil))
	d.twilioBaseURL = srv.URL
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications([]run.NotifyTarget{{Kind: run.NotifyTwilio, To: "+15559999"}}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)
	select {
	case got := <-received:
		if got.to != "+15559999" {
			t.Errorf("To = %q, want the template's recipient +15559999", got.to)
		}
		if got.from != "+15550000" {
			t.Errorf("From = %q, want the server-held sender", got.from)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("AC123:tok"))
		if got.auth != wantAuth {
			t.Errorf("auth = %q, want the server account credentials", got.auth)
		}
		if !strings.Contains(got.path, "AC123") {
			t.Errorf("path = %q, want the server account SID", got.path)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("per-template twilio target did not text")
	}
}

// TestPerTemplateTwilioSkippedWithoutAccount proves that a Twilio target on a run is skipped, not
// fatal, when the server has no Twilio account: a template cannot carry the account secret, so
// without a configured account there is nothing to send through.
func TestPerTemplateTwilioSkippedWithoutAccount(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 2}, nil
		}), nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications([]run.NotifyTarget{{Kind: run.NotifyTwilio, To: "+15559999"}}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := waitTerminal(t, store, created.ID); got.Status != run.StatusFailed {
		t.Fatalf("status = %q, want the run to finish regardless of an unsendable target", got.Status)
	}
}

// TestPerTemplateEmailReachesTargetRecipient proves an email target mails the recipient the template
// names through the server-held SMTP transport, not the server default recipient.
func TestPerTemplateEmailReachesTargetRecipient(t *testing.T) {
	t.Parallel()
	emailer := &captureEmailer{sent: make(chan string, 4)}
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), nil, WithEmail(emailer, false))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications([]run.NotifyTarget{
			{Kind: run.NotifyEmail, To: "oncall@example.com, lead@example.com"},
		}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)
	select {
	case <-emailer.sent:
		want := []string{"oncall@example.com", "lead@example.com"}
		if got := emailer.recipients(); !slices.Equal(got, want) {
			t.Errorf("recipients = %v, want the template's list %v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("per-template email target was not sent")
	}
}
