package dispatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// recordingRouter is a NotificationRouter that sends every event to one webhook and records the
// status of each run snapshot it was asked about.
type recordingRouter struct {
	// url is the webhook every event goes to.
	url string
	// mu guards seen.
	mu sync.Mutex
	// seen are the statuses of the runs the router was asked about, in order.
	seen []run.Status
}

// Targets records the run's status and returns the one webhook target.
func (rr *recordingRouter) Targets(_ context.Context, r *run.Run) []run.NotifyTarget {
	rr.mu.Lock()
	rr.seen = append(rr.seen, r.Status)
	rr.mu.Unlock()
	return []run.NotifyTarget{{Kind: run.NotifyWebhook, URL: rr.url}}
}

// TestNamedTargetsHearEveryEvent pins that a run reaches the named targets attached to it at each
// event: when it starts, when a rule holds it, and when it ends, whether it succeeds or fails. The
// server-wide channels and a template's inline targets never heard a start; a named target attached
// for one does, and the start arrives as a start, not as the failure any status other than
// succeeded used to render as.
func TestNamedTargetsHearEveryEvent(t *testing.T) {
	t.Parallel()
	failing := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 2}, nil
		})
	tests := []struct {
		Runner     roundhouse.Runner
		WantEvents []string
		WantStatus []run.Status
		Held       bool
	}{{ // Test 0: A run that succeeds is announced when it starts and when it ends.
		Runner:     okRunner(),
		WantEvents: []string{"run.started running", "run.finished succeeded"},
		WantStatus: []run.Status{run.StatusRunning, run.StatusSucceeded},
	}, { // Test 1: A run that fails is announced when it starts and when it fails.
		Runner:     failing,
		WantEvents: []string{"run.started running", "run.finished failed"},
		WantStatus: []run.Status{run.StatusRunning, run.StatusFailed},
	}, { // Test 2: A held run is announced when it is held, then started and finished on release.
		Runner: okRunner(), Held: true,
		WantEvents: []string{"run.held pending_approval", "run.started running",
			"run.finished succeeded"},
		WantStatus: []run.Status{run.StatusPendingApproval, run.StatusRunning,
			run.StatusSucceeded},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			hookURL, events := captureEvents(t)
			router := &recordingRouter{url: hookURL}
			opts := []Option{WithNotificationRouter(router), WithNotifyClient(http.DefaultClient)}
			if test.Held {
				opts = append(opts, WithPolicies(holdEverything(t)))
			}
			store := run.NewMemStore()
			d := New(store, test.Runner, nil, opts...)
			defer d.Close()
			created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash),
				run.WithCommand("echo hi"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			var got []string
			if test.Held {
				e := nextEvent(t, events)
				got = append(got, e.Event+" "+string(e.Run.Status))
				if _, err := d.Approve(ctx, created.ID, decider("admin", "user")); err != nil {
					t.Fatalf("Approve() error = %v", err)
				}
			}
			for len(got) < len(test.WantEvents) {
				e := nextEvent(t, events)
				got = append(got, e.Event+" "+string(e.Run.Status))
			}
			// Delivery is best effort and concurrent, like every channel's, so a fast run's start
			// and end may land in either order. Each says which it is, so a receiver can tell.
			byText := cmpopts.SortSlices(func(a, b string) bool { return a < b })
			if diff := cmp.Diff(test.WantEvents, got, byText); diff != "" {
				t.Errorf("events mismatch (-want +got):\n%s", diff)
			}
			noneArrive(t, events, "a run's named target after its last event")
			router.mu.Lock()
			defer router.mu.Unlock()
			if diff := cmp.Diff(test.WantStatus, router.seen,
				cmpopts.SortSlices(func(a, b run.Status) bool { return a < b })); diff != "" {
				t.Errorf("router asked about mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStartedMessagesReadAsStarts pins what each channel says when a run starts. Every formatter
// read any status other than succeeded as a failure, so a start would have arrived with a red
// cross.
func TestStartedMessagesReadAsStarts(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run_go", Playbook: "site.yml", Status: run.StatusRunning}
	tests := []struct {
		Got      string
		WantText string
	}{{ // Test 0: Slack.
		Got: slackMessage(r), WantText: ":arrow_forward: SwitchTender run *site.yml* started.",
	}, { // Test 1: Discord.
		Got: discordMessage(r), WantText: "SwitchTender run **site.yml** started.",
	}, { // Test 2: The ntfy title.
		Got: ntfyHeaders(r)["Title"], WantText: "SwitchTender run site.yml started",
	}, { // Test 3: The email subject.
		Got: emailSubject(r), WantText: "SwitchTender run run_go started",
	}, { // Test 4: The Grafana annotation.
		Got: grafanaText(r), WantText: "SwitchTender run site.yml started.",
	}, { // Test 5: The webhook event.
		Got: webhookEvent(r), WantText: "run.started",
	}, { // Test 6: A start is not raised to high priority on ntfy.
		Got: ntfyHeaders(r)["Priority"], WantText: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantText, test.Got); diff != "" {
				t.Errorf("message mismatch (-want +got):\n%s", diff)
			}
		})
	}
	if body := emailBody(r); strings.Contains(body, "finished") {
		t.Errorf("email body for a start says it finished: %q", body)
	}
}

// TestNamedTargetsSkipChildren pins that a split's shards and a pipeline's steps are not announced
// one by one. The parent is the run a target is attached through, so it is the one announced.
func TestNamedTargetsSkipChildren(t *testing.T) {
	t.Parallel()
	asked := make(chan string, 8)
	router := NotificationRouterFunc(func(_ context.Context, r *run.Run) []run.NotifyTarget {
		asked <- r.ID
		return nil
	})
	d := New(run.NewMemStore(), okRunner(), nil, WithNotificationRouter(router))
	defer d.Close()
	parent := "run_parent"
	d.notifyNamed(&run.Run{ID: "run_child", ParentID: &parent, Status: run.StatusSucceeded})
	d.notifyStarted(&run.Run{ID: "run_child2", ParentID: &parent, Status: run.StatusRunning})
	d.notifyStarted(&run.Run{ID: "run_pending", Status: run.StatusPending})
	noneArrive(t, asked, "the router for a child or a run that has not started")
}
