package notification

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// fakeClock is a settable clock for an outbox under test.
type fakeClock struct {
	// mu guards now.
	mu sync.Mutex
	// now is the current time.
	now time.Time
}

// read returns the current time.
func (c *fakeClock) read(context.Context) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now, nil
}

// advance moves the clock forward.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recordingSender records every message it is handed and answers with the next scripted error for
// its target, nil once the script runs out.
type recordingSender struct {
	// mu guards sent and script.
	mu sync.Mutex
	// sent are the messages, rendered as target url, event, sequence, and note.
	sent []string
	// script holds the errors to answer each target's attempts with, in order, by target url.
	script map[string][]error
}

// send records the message and answers from the script.
func (s *recordingSender) send(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := fmt.Sprintf("%s %s #%d", m.Target.URL, m.Event, m.Seq)
	if m.Note != "" {
		line += " note"
	}
	s.sent = append(s.sent, line)
	if errs := s.script[m.Target.URL]; len(errs) > 0 {
		s.script[m.Target.URL] = errs[1:]
		return errs[0]
	}
	return nil
}

// lines returns what was sent so far.
func (s *recordingSender) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// outboxFixture is a store with targets attached to one template, an outbox over it, and a clock.
type outboxFixture struct {
	// store holds the targets, attachments, events, and deliveries.
	store Store
	// outbox records and delivers.
	outbox *Outbox
	// clock is the outbox's clock.
	clock *fakeClock
}

// newOutboxFixture attaches each named webhook target to template tpl_deploy for every event, and a
// target waiting for its secret for failures.
func newOutboxFixture(t *testing.T, targets ...string) outboxFixture {
	t.Helper()
	store := NewMemStore()
	for _, id := range targets {
		mustTarget(t, store, id, run.NotifyTarget{Kind: run.NotifyWebhook,
			URL: "https://hooks.example.com/" + id})
		for _, ev := range Events {
			mustAttach(t, store, id, KindTemplate, "tpl_deploy", ev)
		}
	}
	if err := store.Save(context.Background(), &Notification{ID: "ntf_shell", Name: "shell",
		Kind: run.NotifySlack, NeedsSecret: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	mustAttach(t, store, "ntf_shell", KindTemplate, "tpl_deploy", EventFailure)
	clock := &fakeClock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	router := NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil)
	return outboxFixture{store: store, clock: clock,
		outbox: NewOutbox(store, router, testSealer{}, nil, WithClock(clock.read))}
}

// runAt returns the template's run at a status.
func runAt(id string, status run.Status) *run.Run {
	return &run.Run{ID: id, Status: status, Source: "template", SourceID: "tpl_deploy"}
}

// drain flushes until nothing more is due, bounded so a bug cannot loop the test forever.
func drain(t *testing.T, f outboxFixture, s *recordingSender) {
	t.Helper()
	for range 50 {
		if f.outbox.Flush(context.Background(), s.send) == 0 {
			return
		}
	}
	t.Fatalf("the outbox never ran out of due deliveries")
}

// TestOutboxDeliversInTheRunsOrder pins the delivery contract: for each run and target, events are
// attempted in the run's order; a failing delivery holds back the ones after it while it is being
// retried, and once its attempts run out it is kept as failed and the next one goes ahead carrying
// the note; other targets never wait for it; and a target waiting for its secret is recorded as
// skipped rather than left out.
func TestOutboxDeliversInTheRunsOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(t, "ntf_a", "ntf_b")
	failing := errors.New("the target answered 503")
	sender := &recordingSender{script: map[string][]error{
		"https://hooks.example.com/ntf_a": {failing, failing, failing, failing, failing},
	}}
	for _, status := range []run.Status{run.StatusRunning, run.StatusFailed} {
		if err := f.outbox.Record(ctx, runAt("run_1", status), Branch{}); err != nil {
			t.Fatalf("Record(%s) error = %v", status, err)
		}
	}
	drain(t, f, sender)
	// ntf_b is delivered both events in order; ntf_a's start failed once and its failure waits.
	want := []string{
		"https://hooks.example.com/ntf_a started #1",
		"https://hooks.example.com/ntf_b started #1",
		"https://hooks.example.com/ntf_b failure #2",
	}
	if diff := cmp.Diff(want, sender.lines(), cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	})); diff != "" {
		t.Fatalf("first pass mismatch (-want +got):\n%s", diff)
	}
	for attempt := 2; attempt <= MaxAttempts; attempt++ {
		f.clock.advance(RetryDelay(attempt - 1))
		drain(t, f, sender)
	}
	got := sender.lines()
	wantTail := []string{
		"https://hooks.example.com/ntf_a started #1",
		"https://hooks.example.com/ntf_a failure #2 note",
	}
	if diff := cmp.Diff(wantTail, got[len(got)-2:]); diff != "" {
		t.Errorf("after the retries ran out mismatch (-want +got):\n%s", diff)
	}
	deliveries, err := f.store.Deliveries(ctx, DeliveryFilter{RunID: "run_1"})
	if err != nil {
		t.Fatalf("Deliveries() error = %v", err)
	}
	status := map[string]string{}
	for _, d := range deliveries {
		status[d.Key()] = fmt.Sprintf("%s %d %s", d.Status, d.Attempts, d.LastError)
	}
	wantStatus := map[string]string{
		"ntf_a/run_1/1": "failed 5 the target answered 503",
		"ntf_a/run_1/2": "delivered 1 ",
		"ntf_b/run_1/1": "delivered 1 ",
		"ntf_b/run_1/2": "delivered 1 ",
		"ntf_shell/run_1/2": "skipped 0 not sent: the target is waiting for its secret to be " +
			"entered",
	}
	if diff := cmp.Diff(wantStatus, status); diff != "" {
		t.Errorf("delivery records mismatch (-want +got):\n%s", diff)
	}
}

// TestOutboxGivesUpAtOnceOnAPermanentFailure pins that a failure no retry can fix, and a target
// deleted before its delivery, are marked failed on the first attempt rather than retried.
func TestOutboxGivesUpAtOnceOnAPermanentFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		Script    []error
		Delete    bool
		WantError string
	}{{ // Test 0: The receiving end refused the request outright.
		Script:    []error{Permanent(errors.New("the target answered 404"))},
		WantError: "the target answered 404",
	}, { // Test 1: The target was deleted after the event was recorded.
		Delete:    true,
		WantError: "the target was deleted before this notification was delivered",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newOutboxFixture(t, "ntf_a")
			sender := &recordingSender{script: map[string][]error{
				"https://hooks.example.com/ntf_a": test.Script}}
			err := f.outbox.Record(ctx, runAt("run_p", run.StatusRunning), Branch{})
			if err != nil {
				t.Fatalf("Record() error = %v", err)
			}
			if test.Delete {
				if err := f.store.Delete(ctx, "ntf_a"); err != nil {
					t.Fatalf("Delete() error = %v", err)
				}
			}
			drain(t, f, sender)
			list, err := f.store.Deliveries(ctx, DeliveryFilter{RunID: "run_p"})
			if err != nil || len(list) != 1 {
				t.Fatalf("Deliveries() = %v, %v, want one", list, err)
			}
			got := fmt.Sprintf("%s %d %s", list[0].Status, list[0].Attempts, list[0].LastError)
			if want := "failed 1 " + test.WantError; got != want {
				t.Errorf("delivery = %q, want %q", got, want)
			}
		})
	}
}

// TestOutboxKeepsSecretsOutOfTheRecord pins that a failure whose text quotes the target's address
// or key is recorded with both masked.
func TestOutboxKeepsSecretsOutOfTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	secretURL := "https://grafana.example.com/hook/s3cr3t-path"
	mustTarget(t, store, "ntf_g", run.NotifyTarget{Kind: run.NotifyGrafana, URL: secretURL,
		Key: "glsa_token_value"})
	mustAttach(t, store, "ntf_g", KindTemplate, "tpl_deploy", EventStarted)
	router := NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil)
	o := NewOutbox(store, router, testSealer{}, nil)
	if err := o.Record(ctx, runAt("run_s", run.StatusRunning), Branch{}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	o.Flush(ctx, func(_ context.Context, m Message) error {
		return Permanent(fmt.Errorf("post %s with %s refused", m.Target.URL, m.Target.Key))
	})
	list, err := store.Deliveries(ctx, DeliveryFilter{RunID: "run_s"})
	if err != nil || len(list) != 1 {
		t.Fatalf("Deliveries() = %v, %v", list, err)
	}
	for _, secret := range []string{"s3cr3t-path", "glsa_token_value"} {
		if strings.Contains(list[0].LastError, secret) {
			t.Errorf("the recorded failure %q holds the secret %q", list[0].LastError, secret)
		}
	}
	if list[0].Status != DeliveryFailed {
		t.Errorf("status = %q, want failed", list[0].Status)
	}
}

// TestOutboxRecordsOnlyWhatHasAnEvent pins what Record leaves alone: a run at no event, a child of
// a split or pipeline, a run with no id, and a run no target is attached for record nothing.
func TestOutboxRecordsOnlyWhatHasAnEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newOutboxFixture(t, "ntf_a")
	parent := "run_parent"
	tests := []struct {
		In *run.Run
	}{
		{In: runAt("run_pending", run.StatusPending)}, // Test 0: Pending is no event.
		{In: &run.Run{ID: "run_child", Status: run.StatusRunning, ParentID: &parent,
			Source: "template", SourceID: "tpl_deploy"}}, // Test 1: A child is not announced.
		{In: runAt("", run.StatusRunning)}, // Test 2: No run to order by.
		{In: &run.Run{ID: "run_other", Status: run.StatusRunning, Source: "template",
			SourceID: "tpl_other"}}, // Test 3: Nothing attached.
	}
	for testNum, test := range tests {
		if err := f.outbox.Record(ctx, test.In, Branch{}); err != nil {
			t.Fatalf("test %d: Record() error = %v", testNum, err)
		}
	}
	list, err := f.store.Deliveries(ctx, DeliveryFilter{})
	if err != nil || len(list) != 0 {
		t.Errorf("Deliveries() = %v, %v, want none recorded", list, err)
	}
}

// TestOutboxServeStopsWithItsContext pins that Serve delivers what is recorded while it runs and
// returns once its context ends.
func TestOutboxServeStopsWithItsContext(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t, "ntf_a")
	f.outbox = NewOutbox(f.store, NewRouter(f.store, testSealer{}, SourceLineage(nil, nil, nil),
		nil), testSealer{}, nil, WithPoll(10*time.Millisecond))
	sender := &recordingSender{script: map[string][]error{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.outbox.Serve(ctx, sender.send)
	}()
	if err := f.outbox.Record(context.Background(), runAt("run_live", run.StatusRunning),
		Branch{}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(sender.lines()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
	if diff := cmp.Diff([]string{"https://hooks.example.com/ntf_a started #1"},
		sender.lines()); diff != "" {
		t.Errorf("Serve delivered mismatch (-want +got):\n%s", diff)
	}
}

// TestOutboxKeepsEachChannelsRules pins that a named target hears what a template's own target of
// the same kind would: a hold never pages, texts, or annotates a dashboard, and PagerDuty pages
// only for a run that failed or was interrupted.
func TestOutboxKeepsEachChannelsRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	mustTarget(t, store, "ntf_pager", run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "rk"})
	mustTarget(t, store, "ntf_dash", run.NotifyTarget{Kind: run.NotifyGrafana,
		URL: "https://grafana.example.com", Key: "tok"})
	mustTarget(t, store, "ntf_mail", run.NotifyTarget{Kind: run.NotifyEmail, To: "a@b.c"})
	for _, id := range []string{"ntf_pager", "ntf_dash", "ntf_mail"} {
		for _, ev := range Events {
			mustAttach(t, store, id, KindTemplate, "tpl_deploy", ev)
		}
	}
	o := NewOutbox(store, NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil),
		testSealer{}, nil)
	tests := []struct {
		Status      run.Status
		WantTargets []string
	}{
		{Status: run.StatusPendingApproval, WantTargets: []string{"ntf_mail"}},       // Test 0.
		{Status: run.StatusRunning, WantTargets: []string{"ntf_dash", "ntf_mail"}},   // Test 1.
		{Status: run.StatusSucceeded, WantTargets: []string{"ntf_dash", "ntf_mail"}}, // Test 2.
		{Status: run.StatusFailed,
			WantTargets: []string{"ntf_dash", "ntf_mail", "ntf_pager"}}, // Test 3.
		{Status: run.StatusCanceled, WantTargets: []string{"ntf_dash", "ntf_mail"}}, // Test 4.
	}
	for testNum, test := range tests {
		id := fmt.Sprintf("run_%d", testNum)
		if err := o.Record(ctx, runAt(id, test.Status), Branch{}); err != nil {
			t.Fatalf("test %d: Record() error = %v", testNum, err)
		}
		list, err := store.Deliveries(ctx, DeliveryFilter{RunID: id})
		if err != nil {
			t.Fatalf("test %d: Deliveries() error = %v", testNum, err)
		}
		got := []string{}
		for _, d := range list {
			got = append(got, d.NotificationID)
		}
		sort.Strings(got)
		if diff := cmp.Diff(test.WantTargets, got); diff != "" {
			t.Errorf("test %d: recorded for mismatch (-want +got):\n%s", testNum, diff)
		}
	}
}

// TestHearsKeepsEveryPathsRules pins which channel kinds a named target's ordered delivery records a
// run for. A hold never pages, texts, or annotates, and a pager hears only a failed or interrupted
// run, the rules every other path keeps, while an attention alert reaches every kind attached for
// it, whatever the status of the run it is about.
func TestHearsKeepsEveryPathsRules(t *testing.T) {
	t.Parallel()
	alert := &run.AttentionNote{ID: "att_1", Summary: "waited too long"}
	tests := []struct {
		Kind      string
		Status    run.Status
		Attention *run.AttentionNote
		WantHears bool
	}{{ // Test 0: A hold reaches a chat channel.
		Kind: run.NotifySlack, Status: run.StatusPendingApproval, WantHears: true,
	}, { // Test 1: A hold never pages.
		Kind: run.NotifyPagerDuty, Status: run.StatusPendingApproval, WantHears: false,
	}, { // Test 2: A hold never texts.
		Kind: run.NotifyTwilio, Status: run.StatusPendingApproval, WantHears: false,
	}, { // Test 3: A hold never annotates a dashboard.
		Kind: run.NotifyGrafana, Status: run.StatusPendingApproval, WantHears: false,
	}, { // Test 4: A success never pages.
		Kind: run.NotifyPagerDuty, Status: run.StatusSucceeded, WantHears: false,
	}, { // Test 5: A failure pages.
		Kind: run.NotifyPagerDuty, Status: run.StatusFailed, WantHears: true,
	}, { // Test 6: An alert on a held run pages.
		Kind: run.NotifyPagerDuty, Status: run.StatusPendingApproval, Attention: alert,
		WantHears: true,
	}, { // Test 7: An alert on a queued run texts.
		Kind: run.NotifyTwilio, Status: run.StatusPending, Attention: alert, WantHears: true,
	}, { // Test 8: An alert on a running run annotates a dashboard.
		Kind: run.NotifyGrafana, Status: run.StatusRunning, Attention: alert, WantHears: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_1", Status: test.Status, Attention: test.Attention}
			if got := Hears(test.Kind, r); got != test.WantHears {
				t.Errorf("Hears(%s, %s) = %t, want %t", test.Kind, test.Status, got, test.WantHears)
			}
		})
	}
}
