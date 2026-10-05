package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// outboxClock is a settable clock for an outbox under test.
type outboxClock struct {
	// mu guards now.
	mu sync.Mutex
	// now is the current time.
	now time.Time
}

// read returns the current time.
func (c *outboxClock) read(context.Context) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now, nil
}

// advance moves the clock forward.
func (c *outboxClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// namedHit is one message a named webhook target received.
type namedHit struct {
	// Event is the webhook event.
	Event string `json:"event"`
	// Delivery identifies the message.
	Delivery namedDelivery `json:"delivery"`
	// Key is the Idempotency-Key header the message carried.
	Key string `json:"-"`
}

// scriptedReceiver serves a named webhook target that answers each request with the next status
// in its script, 200 once the script runs out, and records what it accepted.
type scriptedReceiver struct {
	// mu guards script and accepted.
	mu sync.Mutex
	// script holds the statuses to answer with, in order.
	script []int
	// accepted are the messages it answered 200 to, in arrival order.
	accepted []namedHit
	// seen counts every request, accepted or not.
	seen int
	// refuseOnce, when set, names an event whose first message is answered 503 whenever it
	// arrives, ahead of the script.
	refuseOnce string
}

// serve answers one request from the script.
func (s *scriptedReceiver) serve(w http.ResponseWriter, r *http.Request) {
	var hit namedHit
	_ = json.NewDecoder(r.Body).Decode(&hit)
	hit.Key = r.Header.Get("Idempotency-Key")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen++
	status := http.StatusOK
	switch {
	case s.refuseOnce != "" && hit.Event == s.refuseOnce:
		status, s.refuseOnce = http.StatusServiceUnavailable, ""
	case len(s.script) > 0:
		status, s.script = s.script[0], s.script[1:]
	}
	if status == http.StatusOK {
		s.accepted = append(s.accepted, hit)
	}
	w.WriteHeader(status)
}

// snapshot returns what was accepted and how many requests arrived.
func (s *scriptedReceiver) snapshot() ([]namedHit, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]namedHit(nil), s.accepted...), s.seen
}

// outboxDispatcher builds a dispatcher whose named target, a webhook served by recv, is attached
// to template tpl_named for every event, with an outbox on an in-memory store and a settable clock.
func outboxDispatcher(t *testing.T, recv *scriptedReceiver) (*Dispatcher, named.Store,
	*outboxClock) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(recv.serve))
	t.Cleanup(srv.Close)
	store := named.NewMemStore()
	n := &named.Notification{ID: "ntf_hook", Name: "hook", CreatedAt: time.Now()}
	if err := n.SetTarget(run.NotifyTarget{Kind: run.NotifyWebhook, URL: srv.URL + "/hook"},
		plainSealer{}); err != nil {
		t.Fatalf("SetTarget() error = %v", err)
	}
	if err := store.Save(context.Background(), n); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, ev := range named.Events {
		if err := store.Attach(context.Background(), &named.Attachment{
			ID: named.NewAttachmentID(), NotificationID: n.ID, ObjectKind: named.KindTemplate,
			ObjectID: "tpl_named", Event: ev, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
	}
	clock := &outboxClock{now: time.Now()}
	router := named.NewRouter(store, plainSealer{}, named.SourceLineage(nil, nil, nil), nil)
	outbox := named.NewOutbox(store, router, plainSealer{}, nil, named.WithClock(clock.read),
		named.WithPoll(10*time.Millisecond))
	d := New(run.NewMemStore(), okRunner(), nil, WithNotificationOutbox(outbox),
		WithNotifyClient(http.DefaultClient))
	t.Cleanup(d.Close)
	return d, store, clock
}

// plainSealer seals by prefixing, which is all a test needs from the encryption key.
type plainSealer struct{}

// Enabled reports that the sealer has a key.
func (plainSealer) Enabled() bool { return true }

// Seal prefixes the plaintext.
func (plainSealer) Seal(plain string) (string, error) { return "sealed:" + plain, nil }

// Open strips the prefix.
func (plainSealer) Open(sealed string) (string, error) {
	plain, ok := strings.CutPrefix(sealed, "sealed:")
	if !ok {
		return "", errors.New("not sealed")
	}
	return plain, nil
}

// waitUntil polls until cond holds or fails the test after a bound.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNamedTargetsHearARunInItsOrder pins decision 35 end to end through the dispatcher: a run's
// events reach a named target in the run's order even when the first one fails and is retried,
// because the later one waits for it; every message carries its delivery key and sequence; and once
// an event's attempts run out the next one still goes, carrying the note that an earlier one
// failed.
func TestNamedTargetsHearARunInItsOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Script       []int
		Retries      int
		WantAccepted []string
		WantNote     string
	}{{ // Test 0: The start is refused once, retried, and still arrives before the finish.
		Script: []int{http.StatusServiceUnavailable}, Retries: 1,
		WantAccepted: []string{"run.started #1", "run.finished #2"},
	}, { // Test 1: The start is refused for good, and the finish goes ahead with the note.
		Script:       []int{http.StatusNotFound},
		WantAccepted: []string{"run.finished #2"},
		WantNote:     named.EarlierFailedNote,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			recv := &scriptedReceiver{script: test.Script}
			d, store, clock := outboxDispatcher(t, recv)
			created, err := d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
				run.WithCommand("echo hi"), run.WithSource("template", "tpl_named"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			for attempt := 1; attempt <= test.Retries; attempt++ {
				// The clock moves only once the failed attempt is on record, so the retry it
				// scheduled is the one that comes due.
				waitUntil(t, "the failed attempt on record", func() bool {
					list, err := store.Deliveries(context.Background(),
						named.DeliveryFilter{RunID: created.ID})
					if err != nil {
						return false
					}
					for _, dl := range list {
						if dl.Seq == 1 && dl.Attempts == attempt &&
							dl.Status == named.DeliveryPending {
							return true
						}
					}
					return false
				})
				clock.advance(time.Minute)
				d.outbox.Wake()
			}
			waitUntil(t, "every message", func() bool {
				accepted, _ := recv.snapshot()
				return len(accepted) == len(test.WantAccepted)
			})
			accepted, _ := recv.snapshot()
			var got []string
			for _, hit := range accepted {
				got = append(got, fmt.Sprintf("%s #%d", hit.Event, hit.Delivery.Sequence))
				wantKey := fmt.Sprintf("ntf_hook/%s/%d", created.ID, hit.Delivery.Sequence)
				if hit.Delivery.ID != wantKey || hit.Key != wantKey {
					t.Errorf("message %s carried key %q and header %q, want %q", hit.Event,
						hit.Delivery.ID, hit.Key, wantKey)
				}
			}
			if diff := cmp.Diff(test.WantAccepted, got); diff != "" {
				t.Errorf("accepted mismatch (-want +got):\n%s", diff)
			}
			if last := accepted[len(accepted)-1]; last.Delivery.Note != test.WantNote {
				t.Errorf("the last message's note = %q, want %q", last.Delivery.Note,
					test.WantNote)
			}
			list, err := store.Deliveries(context.Background(),
				named.DeliveryFilter{RunID: created.ID})
			if err != nil {
				t.Fatalf("Deliveries() error = %v", err)
			}
			if test.WantNote != "" {
				failed := 0
				for _, dl := range list {
					if dl.Status == named.DeliveryFailed {
						failed++
					}
				}
				if failed != 1 {
					t.Errorf("failed deliveries recorded = %d, want the one, kept", failed)
				}
			}
		})
	}
}

// TestStepBranchesFollowTheirGraph pins where an approval step's event sits among a workflow's
// events: after every step it comes after, and unordered against a step beside it.
func TestStepBranchesFollowTheirGraph(t *testing.T) {
	t.Parallel()
	approval := func(name string, deps ...string) run.PipelineStep {
		return run.PipelineStep{Name: name, Type: run.StepApproval, DependsOn: deps}
	}
	shell := func(name string, deps ...string) run.PipelineStep {
		return run.PipelineStep{Name: name, Tool: run.ToolBash, Command: "true", DependsOn: deps}
	}
	tests := []struct {
		Steps []run.PipelineStep
		Step  string
		Want  named.Branch
	}{{ // Test 0: A sequence is a chain, so each step comes after every one before it.
		Steps: []run.PipelineStep{shell("build"), approval("first"), shell("ship"),
			approval("second")},
		Step: "second", Want: named.Branch{Step: "second",
			Follows: []string{"build", "first", "ship"}},
	}, { // Test 1: Two approvals beside each other follow their shared parent and not each other.
		Steps: []run.PipelineStep{shell("build"), approval("left", "build"),
			approval("right", "build")},
		Step: "right", Want: named.Branch{Step: "right", Follows: []string{"build"}},
	}, { // Test 2: A step the graph does not hold is placed on its own.
		Steps: []run.PipelineStep{shell("build")}, Step: "gone",
		Want: named.Branch{Step: "gone"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, stepBranch(test.Steps, test.Step)); diff != "" {
				t.Errorf("stepBranch() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSendNamedClassifiesTheAnswer pins what an attempt reports: a 2xx is delivered, a timeout, a
// rate limit, or a server error is retried, and any other answer is a failure no retry fixes.
func TestSendNamedClassifiesTheAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Status        int
		WantErr       bool
		WantPermanent bool
	}{
		{Status: http.StatusNoContent},                                            // Test 0.
		{Status: http.StatusServiceUnavailable, WantErr: true},                    // Test 1.
		{Status: http.StatusTooManyRequests, WantErr: true},                       // Test 2.
		{Status: http.StatusRequestTimeout, WantErr: true},                        // Test 3.
		{Status: http.StatusNotFound, WantErr: true, WantPermanent: true},         // Test 4.
		{Status: http.StatusMovedPermanently, WantErr: true, WantPermanent: true}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			answer := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(test.Status) }
			srv := httptest.NewServer(http.HandlerFunc(answer))
			defer srv.Close()
			d := New(run.NewMemStore(), okRunner(), nil, WithNotifyClient(http.DefaultClient))
			defer d.Close()
			err := d.sendNamed(context.Background(), named.Message{
				Target: run.NotifyTarget{Kind: run.NotifySlack, URL: srv.URL + "/secret-path"},
				Run:    &run.Run{ID: "run_c", Status: run.StatusSucceeded}, Event: "success",
				Seq: 1, Delivery: "ntf_c/run_c/1"})
			if (err != nil) != test.WantErr || named.IsPermanent(err) != test.WantPermanent {
				t.Errorf("sendNamed() = %v, want error %v, permanent %v", err, test.WantErr,
					test.WantPermanent)
			}
			if err != nil && strings.Contains(err.Error(), "secret-path") {
				t.Errorf("the reason quotes the target's address: %v", err)
			}
		})
	}
}

// TestSendNamedCarriesTheNote pins that each channel shows the note in its own place, and that a
// channel the server has no transport for fails for good rather than being retried.
func TestSendNamedCarriesTheNote(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := New(run.NewMemStore(), okRunner(), nil, WithNotifyClient(http.DefaultClient))
	defer d.Close()
	r := &run.Run{ID: "run_n", Status: run.StatusFailed}
	for _, kind := range []string{run.NotifySlack, run.NotifyDiscord, run.NotifyTeams,
		run.NotifyWebhook} {
		err := d.sendNamed(context.Background(), named.Message{
			Target: run.NotifyTarget{Kind: kind, URL: srv.URL}, Run: r, Event: "failure", Seq: 2,
			Delivery: "ntf_n/run_n/2", Note: named.EarlierFailedNote})
		if err != nil {
			t.Fatalf("sendNamed(%s) error = %v", kind, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 4 {
		t.Fatalf("received %d bodies, want 4", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, named.EarlierFailedNote) {
			t.Errorf("message %d does not carry the note: %s", i, body)
		}
	}
	for _, kind := range []string{run.NotifyEmail, run.NotifyTwilio} {
		err := d.sendNamed(context.Background(), named.Message{
			Target: run.NotifyTarget{Kind: kind, To: "ops@example.com"}, Run: r})
		if !named.IsPermanent(err) {
			t.Errorf("sendNamed(%s) with no transport = %v, want a permanent failure", kind, err)
		}
	}
}

// TestSendNamedRendersAnAlertAsEveryPathDoes pins that the ordered delivery to a named pager,
// dashboard, or phone says what the direct path to the same kind says about an attention alert: a
// warning keyed by the alert, an annotation tagged with the alert, and a text leading with it, never
// the run's status line.
func TestSendNamedRendersAnAlertAsEveryPathDoes(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		bodies = map[string]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.URL.Path] = string(raw)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := New(run.NewMemStore(), okRunner(), nil, WithNotifyClient(http.DefaultClient),
		WithTwilio("AC1", "token", "+15550000000", nil))
	defer d.Close()
	d.pagerDutyEndpoint = srv.URL + "/pagerduty"
	d.twilioBaseURL = srv.URL
	r := &run.Run{ID: "run_q", Status: run.StatusPendingApproval, Attention: &run.AttentionNote{
		ID: "att_1", Blocker: "approval_needed", Summary: "Run run_q has waited 2h for approval."}}
	for _, target := range []run.NotifyTarget{
		{Kind: run.NotifyPagerDuty, Key: "named-key"},
		{Kind: run.NotifyGrafana, URL: srv.URL + "/grafana"},
		{Kind: run.NotifyTwilio, To: "+15551111111"},
	} {
		if err := d.sendNamed(context.Background(), named.Message{Target: target, Run: r,
			Event: named.EventAttention, Seq: 1, Delivery: "ntf_a/run_q/1"}); err != nil {
			t.Fatalf("sendNamed(%s) error = %v", target.Kind, err)
		}
	}
	event, ok := pagerDutyEventFor(r, "named-key")
	if !ok {
		t.Fatal("the direct path pages for nothing on an alert")
	}
	page, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode the direct path's page: %v", err)
	}
	ann, err := json.Marshal(grafanaAnnotationFor(r))
	if err != nil {
		t.Fatalf("encode the direct path's annotation: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	got := map[string]string{
		"page":       bodies["/pagerduty"],
		"annotation": bodies["/grafana/api/annotations"],
		"text":       bodies["/2010-04-01/Accounts/AC1/Messages.json"],
	}
	want := map[string]string{
		"page":       string(page),
		"annotation": string(ann),
		"text": url.Values{"To": {"+15551111111"}, "From": {"+15550000000"},
			"Body": {"SwitchTender alert: Run run_q has waited 2h for approval."}}.Encode(),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ordered delivery mismatch against the direct path (-want +got):\n%s", diff)
	}
	if !strings.Contains(got["page"], `"dedup_key":"att_1"`) ||
		!strings.Contains(got["page"], `"severity":"warning"`) {
		t.Errorf("the page is not the alert's warning: %s", got["page"])
	}
}

// TestStepAndAttentionNoticesKeepTheRunsOrder pins that a workflow waiting at an approval step and
// an attention alert raised on it travel the named targets' ordered queue like every other event of
// the run: each carries its own event name and the run's next sequence number, and the alert waits
// for the step's notice when that one is refused once and retried. A pager attached for both events
// hears the alert, which reports a problem, and never the hold, which asks for a decision.
func TestStepAndAttentionNoticesKeepTheRunsOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recv := &scriptedReceiver{refuseOnce: "workflow.step_awaiting_approval"}
	d, store, clock := outboxDispatcher(t, recv)

	// pagerBody is the part of a PagerDuty event this test reads.
	type pagerBody struct {
		// RoutingKey is the integration key paged.
		RoutingKey string `json:"routing_key"`
		// DedupKey collapses repeats.
		DedupKey string `json:"dedup_key"`
		// Payload says what the page is about.
		Payload struct {
			// Severity is warning for an alert.
			Severity string `json:"severity"`
		} `json:"payload"`
	}
	pages := make(chan pagerBody, 8)
	pager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p pagerBody
		if json.NewDecoder(r.Body).Decode(&p) == nil {
			pages <- p
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(pager.Close)
	d.pagerDutyEndpoint = pager.URL
	page := &named.Notification{ID: "ntf_pager", Name: "pager", CreatedAt: time.Now()}
	if err := page.SetTarget(run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "named-key"},
		plainSealer{}); err != nil {
		t.Fatalf("SetTarget() error = %v", err)
	}
	if err := store.Save(ctx, page); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, ev := range []string{named.EventApproval, named.EventAttention} {
		if err := store.Attach(ctx, &named.Attachment{ID: named.NewAttachmentID(),
			NotificationID: page.ID, ObjectKind: named.KindTemplate, ObjectID: "tpl_named",
			Event: ev, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
	}

	parent := &run.Run{ID: "run_flow", Status: run.StatusRunning, Source: "template",
		SourceID: "tpl_named", CreatedAt: time.Now(), Steps: []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "true"},
			{Name: "gate", Type: run.StepApproval, DependsOn: []string{"build"}}}}
	index := 1
	node := &run.Run{ID: "run_gate", ParentID: &parent.ID, StepName: "gate", StepIndex: &index,
		Status: run.StatusPendingApproval, CreatedAt: time.Now()}
	for _, r := range []*run.Run{parent, node} {
		if err := d.store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	d.notifyStepHeld(parent, node)
	alert := parent.Clone()
	alert.Status = run.StatusPendingApproval
	alert.Attention = &run.AttentionNote{ID: "att_flow", Blocker: "approval_needed",
		Summary: "Workflow run_flow has waited 2h at approval step gate."}
	d.NotifyAttention(alert)

	// The step's notice is refused once. The clock moves only once that attempt is on record, so
	// the retry it scheduled is the one that comes due, and the alert behind it has to wait for it.
	waitUntil(t, "the refused attempt on record", func() bool {
		list, err := store.Deliveries(ctx, named.DeliveryFilter{RunID: parent.ID})
		if err != nil {
			return false
		}
		for _, dl := range list {
			if dl.NotificationID == "ntf_hook" && dl.Seq == 1 && dl.Attempts == 1 &&
				dl.Status == named.DeliveryPending {
				return true
			}
		}
		return false
	})
	clock.advance(time.Minute)
	d.outbox.Wake()
	waitUntil(t, "both notices", func() bool {
		accepted, _ := recv.snapshot()
		return len(accepted) == 2
	})
	accepted, seen := recv.snapshot()
	var got []string
	for _, hit := range accepted {
		got = append(got, fmt.Sprintf("%s #%d", hit.Event, hit.Delivery.Sequence))
		if want := fmt.Sprintf("ntf_hook/%s/%d", parent.ID, hit.Delivery.Sequence); hit.Key != want {
			t.Errorf("message %s carried idempotency key %q, want %q", hit.Event, hit.Key, want)
		}
	}
	want := []string{"workflow.step_awaiting_approval #1", "run.needs_attention #2"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("accepted mismatch (-want +got):\n%s", diff)
	}
	if seen != 3 {
		t.Errorf("the webhook was asked %d times, want 3: the refusal, its retry, and the alert", seen)
	}
	select {
	case p := <-pages:
		if p.RoutingKey != "named-key" || p.DedupKey != "att_flow" || p.Payload.Severity != "warning" {
			t.Errorf("page = %+v, want the alert on named-key as a warning keyed by the alert", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pager attached for the attention event was never paged")
	}
	noneArrive(t, pages, "a second page, which only the hold could have sent")
}
