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

// TestDispatcherNotifiesGrafana verifies a finished run posts an annotation to the Grafana
// annotations API with a bearer token, the run summary, and status tags.
func TestDispatcherNotifiesGrafana(t *testing.T) {
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
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithGrafana([]string{srv.URL}, "gf-tok"), WithNotifyClient(http.DefaultClient))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case got := <-received:
		if got.path != "/api/annotations" {
			t.Errorf("path = %q, want /api/annotations", got.path)
		}
		if got.auth != "Bearer gf-tok" {
			t.Errorf("auth = %q, want Bearer gf-tok", got.auth)
		}
		if !strings.Contains(got.ann.Text, "play.yml") || !strings.Contains(got.ann.Text, "succeeded") {
			t.Errorf("annotation text = %q, want it to mention play.yml and succeeded", got.ann.Text)
		}
		if len(got.ann.Tags) < 2 || got.ann.Tags[0] != "switchtender" || got.ann.Tags[1] != "succeeded" {
			t.Errorf("annotation tags = %v, want [switchtender succeeded]", got.ann.Tags)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("grafana never received the annotation")
	}
}
