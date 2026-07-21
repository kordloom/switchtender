package dispatch

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDispatcherNotifiesTwilio verifies a failed run texts each recipient through the Twilio Messages
// API with basic auth and the sender, recipient, and message body, and that a succeeded run texts no
// one.
func TestDispatcherNotifiesTwilio(t *testing.T) {
	type capture struct {
		path, auth string
		form       url.Values
	}
	received := make(chan capture, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		received <- capture{path: r.URL.Path, auth: r.Header.Get("Authorization"), form: form}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	orig := twilioBaseURL
	twilioBaseURL = srv.URL
	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 2}, nil
		})
	d := New(store, runner, nil, WithTwilio("AC123", "tok", "+15550000", []string{"+15551111"}))
	defer func() {
		d.Close()
		twilioBaseURL = orig
	}()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := waitTerminal(t, store, created.ID); got.Status != run.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}

	select {
	case got := <-received:
		if got.path != "/2010-04-01/Accounts/AC123/Messages.json" {
			t.Errorf("path = %q, want the Messages endpoint for AC123", got.path)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("AC123:tok"))
		if got.auth != wantAuth {
			t.Errorf("auth = %q, want basic AC123:tok", got.auth)
		}
		if got.form.Get("To") != "+15551111" || got.form.Get("From") != "+15550000" {
			t.Errorf("to/from = %q/%q, want the recipient and sender",
				got.form.Get("To"), got.form.Get("From"))
		}
		if !strings.Contains(got.form.Get("Body"), "play.yml") ||
			!strings.Contains(got.form.Get("Body"), "failed") {
			t.Errorf("body = %q, want it to mention play.yml and failed", got.form.Get("Body"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("twilio never received the SMS")
	}

	// A succeeded run is not an alert, so it texts no one.
	d.notifyTwilio(&run.Run{ID: "run_ok", Status: run.StatusSucceeded})
	select {
	case <-received:
		t.Error("succeeded run texted twilio")
	case <-time.After(150 * time.Millisecond):
	}
}
