package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// awaitingRun returns a workflow waiting at an approval step, as notifyStepHeld hands it on.
func awaitingRun() *run.Run {
	r := heldRun()
	r.ID, r.Playbook, r.Kind = "run_wf", "release", run.KindPipeline
	r.HeldByPolicy = `approval step "gate"`
	r.AwaitingStep = &run.AwaitingStep{ID: "run_gate", Name: "gate", Description: "Ship it?",
		OnApprove: []string{"deploy"}, OnDeny: []string{"notify"}}
	return r
}

// attentionRun returns a run carrying an attention alert, as the monitor hands it on.
func attentionRun() *run.Run {
	return &run.Run{ID: "run_q", Playbook: "site.yml", Status: run.StatusPending,
		Attention: &run.AttentionNote{ID: "att_1", Blocker: "no_worker",
			Summary:   "Run site.yml has waited 17m for a worker: none serves queue \"prod\".",
			WhoCanAct: "An admin: connect a worker that serves queue \"prod\".",
			Next:      "It starts automatically when a worker that serves queue \"prod\" appears.",
			Path:      "/ui/?attention=no_worker"}}
}

// TestNewEventsSayWhatTheyAre pins what every channel says for the two notices a run's status
// cannot express: a workflow waiting at an approval step, named as a step with what each answer
// runs next rather than as a held run, and an attention alert. A whole held run keeps its own
// wording.
func TestNewEventsSayWhatTheyAre(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In          *run.Run
		WantEvent   string
		WantSlack   string
		WantSubject string
		WantNtfy    string
		WantTitle   string
		WantBody    string
	}{{ // Test 0: A whole held run is still a held run.
		In: heldRun(), WantEvent: "run.held", WantSlack: "is waiting for approval.",
		WantSubject: "is waiting for approval", WantNtfy: "Held by", WantTitle: "is waiting for approval",
		WantBody: "Run run_held is waiting for approval.",
	}, { // Test 1: A workflow at a step names the step and what each answer runs.
		In: awaitingRun(), WantEvent: "workflow.step_awaiting_approval",
		WantSlack: "is waiting at approval step \"gate\". Ship it? Approving runs deploy. " +
			"Denying runs notify.",
		WantSubject: "is waiting at approval step gate",
		WantNtfy:    "Approving runs deploy. Denying runs notify.",
		WantTitle:   "is waiting at approval step gate",
		WantBody:    "POST /v1/runs/run_gate/approve",
	}, { // Test 2: An attention alert leads with its summary and who can act.
		In: attentionRun(), WantEvent: "run.needs_attention",
		WantSlack:   "SwitchTender alert: Run site.yml has waited 17m for a worker",
		WantSubject: "SwitchTender alert: run run_q needs attention",
		WantNtfy:    "An admin: connect a worker",
		WantTitle:   "SwitchTender alert: site.yml needs attention",
		WantBody:    "See everything that needs attention at /ui/?attention=no_worker",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantEvent, webhookEvent(test.In)); diff != "" {
				t.Errorf("webhook event mismatch (-want +got):\n%s", diff)
			}
			card, err := json.Marshal(teamsCardPayload(test.In))
			if err != nil {
				t.Fatalf("Marshal(teams) error = %v", err)
			}
			checks := []struct {
				// what names the channel.
				what string
				// got is what the channel says.
				got string
				// want is a part it must say.
				want string
			}{
				{"slack", slackMessage(test.In), test.WantSlack},
				{"discord", discordMessage(test.In), test.WantSlack},
				{"email subject", emailSubject(test.In), test.WantSubject},
				{"email body", emailBody(test.In), test.WantBody},
				{"ntfy body", ntfyBody(test.In), test.WantNtfy},
				{"ntfy title", ntfyHeaders(test.In)["Title"], test.WantTitle},
				{"teams", string(card), test.WantTitle},
			}
			for _, c := range checks {
				// Discord bolds with two asterisks where Slack uses one, so the bold label is not
				// part of what both are held to.
				if !strings.Contains(c.got, c.want) {
					t.Errorf("%s says %q, want it to contain %q", c.what, c.got, c.want)
				}
			}
		})
	}
}

// TestNotifyAttentionRoutesTheAlert pins where an attention alert goes: the server-wide webhook as
// its own event, the run's own chat and webhook targets but not its own pager, and a named target
// attached for the attention event on every kind, a pager included, since attaching it for that
// event is the request to be paged. The router is asked about the run with the alert on it, which
// is how it finds the targets attached for the attention event.
func TestNotifyAttentionRoutesTheAlert(t *testing.T) {
	t.Parallel()
	hookURL, events := captureEvents(t)
	inlineURL, inline := captureEvents(t)
	// pagerBody records each PagerDuty event's body, so the test can tell the named page from an
	// inline one that must never arrive.
	type pagerBody struct {
		// RoutingKey is the integration key paged.
		RoutingKey string `json:"routing_key"`
		// DedupKey collapses repeats.
		DedupKey string `json:"dedup_key"`
	}
	pages := make(chan pagerBody, 8)
	pager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p pagerBody
		raw, _ := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &p) == nil {
			pages <- p
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(pager.Close)
	asked := make(chan string, 4)
	router := NotificationRouterFunc(func(_ context.Context, r *run.Run) []run.NotifyTarget {
		if r.Attention == nil {
			return nil
		}
		asked <- r.Attention.ID
		return []run.NotifyTarget{{Kind: run.NotifyPagerDuty, Key: "named-key"}}
	})
	d := New(run.NewMemStore(), &commandRecorder{}, nil, WithNoJanitor(),
		WithWebhooks([]string{hookURL}), WithNotificationRouter(router),
		WithNotifyClient(http.DefaultClient))
	d.pagerDutyEndpoint = pager.URL
	defer d.Close()
	alert := attentionRun()
	alert.Notifications = []run.NotifyTarget{
		{Kind: run.NotifyWebhook, URL: inlineURL, OnFailure: true},
		{Kind: run.NotifyPagerDuty, Key: "inline-key"},
	}
	d.NotifyAttention(alert)

	if diff := cmp.Diff("run.needs_attention", nextEvent(t, events).Event); diff != "" {
		t.Errorf("server-wide webhook event mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("run.needs_attention", nextEvent(t, inline).Event); diff != "" {
		t.Errorf("the run's own on-failure target event mismatch (-want +got):\n%s", diff)
	}
	select {
	case id := <-asked:
		if diff := cmp.Diff("att_1", id); diff != "" {
			t.Errorf("router asked about the wrong alert (-want +got):\n%s", diff)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the router was never asked for the targets attached for the alert")
	}
	select {
	case p := <-pages:
		want := pagerBody{RoutingKey: "named-key", DedupKey: "att_1"}
		if diff := cmp.Diff(want, p); diff != "" {
			t.Errorf("page mismatch (-want +got):\n%s", diff)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the named pager target attached for the alert was never paged")
	}
	noneArrive(t, pages, "the run's own pager target, which never hears an alert")
}

// TestNotifyAttentionIsEmailedWhateverNotifyOnSays pins that an alert reaches the server's email
// even when the install mails failures only, since an alert reports a problem, while a hold on the
// same install is still not mailed.
func TestNotifyAttentionIsEmailedWhateverNotifyOnSays(t *testing.T) {
	t.Parallel()
	mail := &captureEmailer{sent: make(chan string, 4)}
	d := New(run.NewMemStore(), &commandRecorder{}, nil, WithNoJanitor(), WithEmail(mail, true))
	defer d.Close()
	d.notifyHeld(heldRun())
	d.NotifyAttention(attentionRun())
	select {
	case subject := <-mail.sent:
		if diff := cmp.Diff("SwitchTender alert: run run_q needs attention", subject); diff != "" {
			t.Errorf("subject mismatch (-want +got):\n%s", diff)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the alert was not emailed on an install that mails failures only")
	}
	noneArrive(t, mail.sent, "a hold on an install that mails failures only")
}

// TestNotifyAttentionIgnoresARunWithoutAnAlert pins that the alert entry delivers nothing for a run
// that carries no alert, and nothing for a child run, which is announced through its parent.
func TestNotifyAttentionIgnoresARunWithoutAnAlert(t *testing.T) {
	t.Parallel()
	hookURL, events := captureEvents(t)
	d := New(run.NewMemStore(), &commandRecorder{}, nil, WithNoJanitor(),
		WithWebhooks([]string{hookURL}), WithNotifyClient(http.DefaultClient))
	defer d.Close()
	child := attentionRun()
	parent := "run_parent"
	child.ParentID = &parent
	d.NotifyAttention(&run.Run{ID: "run_plain", Status: run.StatusPending})
	d.NotifyAttention(child)
	d.NotifyAttention(nil)
	noneArrive(t, events, "a run that carries no alert")
}

// presenceSpy records the reports a dispatcher makes.
type presenceSpy struct {
	// mu guards got.
	mu sync.Mutex
	// got are the reports, in order.
	got []presenceReport
	// fail makes every report fail, to prove a failure does not stop the dispatcher.
	fail bool
}

// presenceReport is one report a dispatcher made.
type presenceReport struct {
	// Owner is the lease name.
	Owner string
	// Queues are the queues it serves.
	Queues []string
	// Slots is its pool size.
	Slots int
}

// NoteWorker records the report.
func (p *presenceSpy) NoteWorker(_ context.Context, owner string, queues []string, slots int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, presenceReport{Owner: owner, Queues: queues, Slots: slots})
	if p.fail {
		return errors.New("store unavailable")
	}
	return nil
}

// reports returns what was recorded so far.
func (p *presenceSpy) reports() []presenceReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]presenceReport(nil), p.got...)
}

// TestDispatcherReportsItsPresence pins that a dispatcher reports itself polling for work, with its
// queues and its pool size, and that one whose claim gate refuses work does not report itself as
// serving anything while that lasts.
func TestDispatcherReportsItsPresence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Gate        func() error
		Fail        bool
		WantReports []presenceReport
	}{{ // Test 0: A dispatcher reports its owner, queues, and pool size at once.
		WantReports: []presenceReport{{Owner: "worker-a", Queues: []string{"prod", "dmz"}, Slots: 3}},
	}, { // Test 1: A refused claim gate reports nothing.
		Gate: func() error { return errors.New("not licensed for workers") },
	}, { // Test 2: A report that fails is logged and the dispatcher keeps running.
		Fail:        true,
		WantReports: []presenceReport{{Owner: "worker-a", Queues: []string{"prod", "dmz"}, Slots: 3}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &presenceSpy{fail: test.Fail}
			opts := []Option{WithNoJanitor(), WithOwner("worker-a"), WithWorkers(3),
				WithQueues([]string{"prod", "dmz"}), WithPresence(spy)}
			if test.Gate != nil {
				opts = append(opts, WithClaimGate(test.Gate))
			}
			d := New(run.NewMemStore(), &commandRecorder{}, nil, opts...)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && len(spy.reports()) == 0 && test.WantReports != nil {
				time.Sleep(5 * time.Millisecond)
			}
			if test.WantReports == nil {
				time.Sleep(100 * time.Millisecond)
			}
			d.Close()
			if diff := cmp.Diff(test.WantReports, spy.reports(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reports mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// slotStore is a run store that records the pool size a dispatcher tells it, the way a relay Client
// passes it on with each claim.
type slotStore struct {
	run.Store
	// mu guards slots.
	mu sync.Mutex
	// slots is the pool size it was told.
	slots int
}

// SetClaimSlots records n.
func (s *slotStore) SetClaimSlots(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots = n
}

// TestDispatcherTellsARelayStoreItsSlots pins that a dispatcher running against a store that sends
// its pool size with each claim tells it that size, so the control node can tell a full relay
// worker from a missing one.
func TestDispatcherTellsARelayStoreItsSlots(t *testing.T) {
	t.Parallel()
	store := &slotStore{Store: run.NewMemStore()}
	d := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithWorkers(6))
	d.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	if diff := cmp.Diff(6, store.slots); diff != "" {
		t.Errorf("slots mismatch (-want +got):\n%s", diff)
	}
}

// TestAnApprovalAgeAlertReachesFailureOnlyTargets pins that a run's own target that asked for
// failures only hears an attention alert even on a held run, whose hold it never hears, since an
// alert reports trouble the way a failure does.
func TestAnApprovalAgeAlertReachesFailureOnlyTargets(t *testing.T) {
	t.Parallel()
	hookURL, events := captureEvents(t)
	d := New(run.NewMemStore(), &commandRecorder{}, nil, WithNoJanitor(),
		WithNotifyClient(http.DefaultClient))
	defer d.Close()
	held := heldRun()
	held.Notifications = []run.NotifyTarget{{Kind: run.NotifyWebhook, URL: hookURL, OnFailure: true}}
	d.notifyHeld(held)
	noneArrive(t, events, "a failure-only target hearing a hold")
	alert := held.Clone()
	alert.Attention = &run.AttentionNote{ID: "att_2", Blocker: "approval_needed",
		Summary: "Run run_held has waited 5h for approval."}
	d.NotifyAttention(alert)
	if diff := cmp.Diff("run.needs_attention", nextEvent(t, events).Event); diff != "" {
		t.Errorf("failure-only target event mismatch (-want +got):\n%s", diff)
	}
}
