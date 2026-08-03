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

// TestDispatcherNotifiesPagerDuty verifies a failed run triggers a PagerDuty incident with the right
// routing key, dedup key, and severity, and that a succeeded run pages no one.
func TestDispatcherNotifiesPagerDuty(t *testing.T) {
	t.Parallel()
	received := make(chan pagerDutyEvent, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e pagerDutyEvent
		if err := json.NewDecoder(r.Body).Decode(&e); err == nil {
			received <- e
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 2}, nil
		})
	d := New(store, runner, nil, WithPagerDuty([]string{"rk-1"}))
	d.pagerDutyEndpoint = srv.URL
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := waitTerminal(t, store, created.ID); got.Status != run.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}

	select {
	case e := <-received:
		if e.RoutingKey != "rk-1" || e.EventAction != "trigger" || e.DedupKey != created.ID {
			t.Errorf("event = %+v, want a trigger for rk-1 deduped on the run id", e)
		}
		if e.Payload.Severity != "error" || !strings.Contains(e.Payload.Summary, "play.yml") {
			t.Errorf("payload = %+v, want error severity mentioning play.yml", e.Payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pagerduty never received the incident")
	}

	// A succeeded run is not an incident, so it pages no one.
	d.notifyPagerDuty(&run.Run{ID: "run_ok", Status: run.StatusSucceeded})
	select {
	case e := <-received:
		t.Errorf("succeeded run paged pagerduty: %+v", e)
	case <-time.After(150 * time.Millisecond):
	}
}
