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

// TestANotificationTargetCannotReachTheServerItself pins the refusal on the path a request takes,
// not on the helper that implements it.
//
// A notification target rides along with a run, so anyone who may start a run may name the address
// the server then connects to. Pointed at the loopback interface that is a request the server makes
// to itself, reaching whatever listens there because it assumes only local processes can. This
// builds a Dispatcher with no client override, which is what a real install runs, and serves the
// target on loopback: the delivery must not arrive.
func TestANotificationTargetCannotReachTheServerItself(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{}, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	// No WithNotifyClient: this is the guarded default a real install uses.
	d := New(store, runner, nil, WithNoJanitor())
	defer d.Close()

	// Every spelling of the same address, because a check on the URL text would catch only the
	// first. The dialer resolves before the refusal runs, so all of these arrive at it as 127.0.0.1.
	port := hook.URL[strings.LastIndex(hook.URL, ":")+1:]
	targets := []run.NotifyTarget{
		{Kind: run.NotifyWebhook, URL: hook.URL},
		{Kind: run.NotifyWebhook, URL: "http://127.1:" + port + "/"},
		{Kind: run.NotifyWebhook, URL: "http://2130706433:" + port + "/"},
		{Kind: run.NotifyWebhook, URL: "http://localhost:" + port + "/"},
	}
	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications(targets))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	// The delivery is attempted in the background, so give it longer than it would need to succeed.
	select {
	case <-arrived:
		t.Fatal("a notification target on the loopback interface was delivered, so a run submitter " +
			"can make this server issue requests to itself")
	case <-time.After(2 * time.Second):
	}
}

// TestAnOffHostNotificationTargetStillArrives pins that the refusal did not simply break delivery.
// Without this the test above would pass on a Dispatcher that notifies nobody at all.
func TestAnOffHostNotificationTargetStillArrives(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{}, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	// The client a test injects stands in for a target that is not this host.
	d := New(store, runner, nil, WithNoJanitor(), WithNotifyClient(http.DefaultClient))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithNotifications([]run.NotifyTarget{{Kind: run.NotifyWebhook, URL: hook.URL}}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("an ordinary notification target was never delivered")
	}
}
