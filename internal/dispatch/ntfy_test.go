package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

func TestNtfyBody(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	if got := ntfyBody(&run.Run{
		Status: run.StatusSucceeded, StartedAt: &start, EndedAt: &end,
	}); got != "succeeded in 1m30s" {
		t.Errorf("ntfyBody(ok) = %q, want 'succeeded in 1m30s'", got)
	}
	if got := ntfyBody(&run.Run{
		Status: run.StatusFailed, Error: "exit status 2", StartedAt: &start, EndedAt: &end,
	}); got != "failed in 1m30s\nexit status 2" {
		t.Errorf("ntfyBody(fail) = %q, want the error appended", got)
	}
}

func TestDispatcherNotifiesNtfy(t *testing.T) {
	t.Parallel()
	type capture struct{ title, tags, auth, body string }
	received := make(chan capture, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- capture{
			title: r.Header.Get("Title"), tags: r.Header.Get("Tags"),
			auth: r.Header.Get("Authorization"), body: string(b),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithNtfy([]string{hook.URL}, "tok"))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case got := <-received:
		if !strings.Contains(got.title, "play.yml") || !strings.Contains(got.title, "succeeded") {
			t.Errorf("ntfy title = %q, want it to mention play.yml and succeeded", got.title)
		}
		if got.tags != "white_check_mark" {
			t.Errorf("ntfy tags = %q, want white_check_mark", got.tags)
		}
		if got.auth != "Bearer tok" {
			t.Errorf("ntfy auth = %q, want Bearer tok", got.auth)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ntfy topic never received the notification")
	}
}
