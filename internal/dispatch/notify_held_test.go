package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// heldRule is the rule name the held fixtures carry, long enough to show it is quoted whole.
const heldRule = "terraform destroys need a second approver (plan destroys 3, limit 0)"

// heldRun returns a run a rule is holding, carrying the fields a held notification reports.
func heldRun() *run.Run {
	return &run.Run{
		ID: "run_held", Tool: run.ToolTerraform, Command: "infra/legacy-network",
		Status: run.StatusPendingApproval, Actor: "deploy-bot", HeldByPolicy: heldRule,
	}
}

// heldEvent is the part of a webhook payload these tests read.
type heldEvent struct {
	// Event is run.held or run.finished.
	Event string `json:"event"`
	// Run carries the id and status of the run the event is about.
	Run struct {
		// ID is the run's id.
		ID string `json:"id"`
		// Status is the run's status when the event was sent.
		Status run.Status `json:"status"`
	} `json:"run"`
}

// captureEvents serves a webhook that records every event it receives.
func captureEvents(t *testing.T) (string, chan heldEvent) {
	t.Helper()
	events := make(chan heldEvent, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e heldEvent
		if err := json.NewDecoder(r.Body).Decode(&e); err == nil {
			events <- e
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, events
}

// captureHits serves an endpoint that records one signal per request, for a channel whose payload
// does not matter because it must not be called at all.
func captureHits(t *testing.T) (string, chan struct{}) {
	t.Helper()
	hits := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

// nextEvent waits for one webhook event.
func nextEvent(t *testing.T, events chan heldEvent) heldEvent {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook event arrived")
		return heldEvent{}
	}
}

// noneArrive fails if anything arrives on ch within a short wait.
func noneArrive[T any](t *testing.T, ch chan T, what string) {
	t.Helper()
	select {
	case got := <-ch:
		t.Errorf("%s: got %+v, want nothing", what, got)
	case <-time.After(300 * time.Millisecond):
	}
}

// holdEverything returns a policy store whose one empty rule holds every run.
func holdEverything(t *testing.T) policy.Store {
	t.Helper()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), policy.NewPolicy("hold-everything")); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}
	return policies
}

// TestHeldMessagesSayWhatIsWaiting pins what each channel says about a held run. A hold is a
// question for whoever reads it, so the message has to say that a decision is waiting, which rule
// is asking for it, and who wants the change, and never render the hold as a failure it is not.
func TestHeldMessagesSayWhatIsWaiting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		In          *run.Run
		WantSlack   string
		WantDiscord string
		WantSubject string
		WantBody    []string
		WantFacts   []string
		// WantNtfy is the ntfy body, which follows a title that already says the run waits.
		WantNtfy string
	}{{ // Test 0: A held run names its rule, quoted whole, and who asked for it.
		Name: "rule and requester",
		In:   heldRun(),
		WantSlack: `SwitchTender run *terraform run_held* is waiting for approval. Held by "` +
			heldRule + `". Requested by deploy-bot.`,
		WantDiscord: `SwitchTender run **terraform run_held** is waiting for approval. Held by "` +
			heldRule + `". Requested by deploy-bot.`,
		WantSubject: "SwitchTender run run_held is waiting for approval",
		WantBody: []string{"Run run_held is waiting for approval.", "Held by: " + heldRule,
			"Requested by: deploy-bot", "POST /v1/runs/run_held/approve",
			"POST /v1/runs/run_held/reject"},
		WantFacts: []string{"Held by", "Requested by"},
		WantNtfy:  `Held by "` + heldRule + `". Requested by deploy-bot.`,
	}, { // Test 1: A hold with no rule or requester recorded invents neither.
		Name:        "no rule and no requester",
		In:          &run.Run{ID: "run_bare", Playbook: "site.yml", Status: run.StatusPendingApproval},
		WantSlack:   "SwitchTender run *site.yml* is waiting for approval.",
		WantDiscord: "SwitchTender run **site.yml** is waiting for approval.",
		WantSubject: "SwitchTender run run_bare is waiting for approval",
		WantBody:    []string{"Run run_bare is waiting for approval.", "Run: site.yml"},
		WantNtfy:    "A person has to approve it before it runs.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantSlack, slackMessage(test.In)); diff != "" {
				t.Errorf("slackMessage mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantDiscord, discordMessage(test.In)); diff != "" {
				t.Errorf("discordMessage mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSubject, emailSubject(test.In)); diff != "" {
				t.Errorf("emailSubject mismatch (-want +got):\n%s", diff)
			}
			body := emailBody(test.In)
			for _, want := range test.WantBody {
				if !strings.Contains(body, want) {
					t.Errorf("emailBody does not say %q:\n%s", want, body)
				}
			}
			if body := emailBody(test.In); strings.Contains(body, "finished") {
				t.Errorf("emailBody reports a held run as finished:\n%s", body)
			}
			if got := webhookEvent(test.In); got != "run.held" {
				t.Errorf("webhookEvent() = %q, want run.held", got)
			}
			headers := ntfyHeaders(test.In)
			if !strings.HasSuffix(headers["Title"], "is waiting for approval") ||
				headers["Tags"] != "hourglass" || headers["Priority"] != "high" {
				t.Errorf("ntfyHeaders() = %v, want a waiting title, the hourglass tag, and high "+
					"priority", headers)
			}
			// The body is read under the title, so it is whole sentences, never the rest of one.
			if diff := cmp.Diff(test.WantNtfy, ntfyBody(test.In)); diff != "" {
				t.Errorf("ntfyBody mismatch (-want +got):\n%s", diff)
			}
			card := teamsCardPayload(test.In).Attachments[0].Content
			title, _ := card.Body[0]["text"].(string)
			if !strings.HasSuffix(title, "is waiting for approval") || card.Body[0]["color"] != "Warning" {
				t.Errorf("teams title = %q color %v, want a waiting title in the warning color",
					title, card.Body[0]["color"])
			}
			facts, _ := card.Body[1]["facts"].([]map[string]string)
			var names []string
			for _, f := range facts {
				names = append(names, f["title"])
			}
			for _, want := range test.WantFacts {
				if !strings.Contains(strings.Join(names, ","), want) {
					t.Errorf("teams facts %v carry no %q", names, want)
				}
			}
		})
	}
}

// TestAHeldRunIsAnnouncedOnceWhereFinishedRunsGo covers the whole life of a held run on the
// channels. A channel heard about a run only when it finished, and a held run finishes only after
// somebody decides on it, so the person who could decide was never told. It has to be told exactly
// once, the retry of the same request must not say it again, the decision's outcome still follows,
// and no pager hears any of it, since a hold is a question rather than an incident.
func TestAHeldRunIsAnnouncedOnceWhereFinishedRunsGo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hookURL, events := captureEvents(t)
	slack := make(chan string, 8)
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
			slack <- p.Text
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()
	pagerURL, pages := captureHits(t)

	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithPolicies(holdEverything(t)),
		WithWebhooks([]string{hookURL}), WithSlack([]string{slackSrv.URL}),
		WithPagerDuty([]string{"team-rk"}), WithNotifyClient(http.DefaultClient))
	d.pagerDutyEndpoint = pagerURL
	defer d.Close()

	submit := func() *run.Run {
		t.Helper()
		created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("echo hi"),
			run.WithActor("deploy-bot"), run.WithIdempotencyKey("held-once"))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		return created
	}

	// Test 0: the hold is announced on the webhook and in chat, naming the rule and the requester.
	held := submit()
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("submitted run is %s, want it held", held.Status)
	}
	if e := nextEvent(t, events); e.Event != "run.held" || e.Run.ID != held.ID {
		t.Errorf("webhook event = %+v, want run.held for %s", e, held.ID)
	}
	select {
	case msg := <-slack:
		for _, want := range []string{"is waiting for approval", `Held by "hold-everything"`,
			"Requested by deploy-bot"} {
			if !strings.Contains(msg, want) {
				t.Errorf("slack message %q does not say %q", msg, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hold was not announced in chat")
	}

	// Test 1: a retry carrying the same key resolves to the held run and announces nothing again.
	if again := submit(); again.ID != held.ID {
		t.Fatalf("retry created %s, want the original %s", again.ID, held.ID)
	}
	noneArrive(t, events, "a retry of the held request")

	// Test 2: once released, the run executes and its outcome follows the hold.
	if _, err := d.Approve(ctx, held.ID, decider("admin", "user")); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if e := nextEvent(t, events); e.Event != "run.finished" || e.Run.Status != run.StatusSucceeded {
		t.Errorf("webhook event = %+v, want run.finished succeeded", e)
	}

	// Test 3: no pager heard the hold or the success.
	noneArrive(t, pages, "the pager")
}

// TestAHeldPipelineIsAnnouncedOnceForTheWholeGraph pins that a held pipeline is announced as the
// one decision it is. The approver releases the parent, not each step, so a message per step would
// be several requests for one decision.
func TestAHeldPipelineIsAnnouncedOnceForTheWholeGraph(t *testing.T) {
	t.Parallel()
	hookURL, events := captureEvents(t)
	d := New(run.NewMemStore(), okRunner(), nil, WithPolicies(holdEverything(t)),
		WithWebhooks([]string{hookURL}), WithNotifyClient(http.DefaultClient))
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "rollout", "", []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "echo build"},
		{Name: "ship", Tool: run.ToolBash, Command: "echo ship"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("pipeline is %s, want it held", parent.Status)
	}
	if e := nextEvent(t, events); e.Event != "run.held" || e.Run.ID != parent.ID {
		t.Errorf("webhook event = %+v, want run.held for the pipeline %s", e, parent.ID)
	}
	noneArrive(t, events, "a second announcement of the held pipeline")
}

// TestHeldRunTargetsSkipPagersAndFailureOnlyTargets pins where a template's own targets send a
// hold. A target that asked for failures only must not hear it, and a pager, a dashboard
// annotation, or a text message must not either, while an ordinary webhook and an email recipient
// are told. The same targets are then shown a failure, so each silence is the code deciding and
// not a target that could never have fired.
func TestHeldRunTargetsSkipPagersAndFailureOnlyTargets(t *testing.T) {
	t.Parallel()
	allURL, all := captureEvents(t)
	failURL, failOnly := captureEvents(t)
	pagerURL, pages := captureHits(t)
	grafanaURL, annotations := captureHits(t)
	twilioURL, texts := captureHits(t)
	emailer := &captureEmailer{sent: make(chan string, 4)}
	// The account a text target sends through. It names no server-wide recipient, so any text
	// here came from the run's own target.
	d := New(run.NewMemStore(), okRunner(), nil, WithNotifyClient(http.DefaultClient),
		WithEmail(emailer, false), WithTwilio("AC0000", "twilio-token", "+15550000000", nil))
	d.pagerDutyEndpoint = pagerURL
	d.twilioBaseURL = twilioURL
	defer d.Close()

	r := heldRun()
	r.Notifications = []run.NotifyTarget{
		{Kind: run.NotifyWebhook, URL: allURL},
		{Kind: run.NotifyWebhook, URL: failURL, OnFailure: true},
		{Kind: run.NotifyPagerDuty, Key: "team-rk"},
		{Kind: run.NotifyGrafana, URL: grafanaURL, Key: "grafana-token"},
		{Kind: run.NotifyTwilio, To: "+15552222222"},
		{Kind: run.NotifyEmail, To: "ops@example.com"},
	}
	d.notifyRunTargets(r)
	d.notifyWG.Wait()

	// Test 0: the ordinary webhook and the email recipient are told.
	if e := nextEvent(t, all); e.Event != "run.held" {
		t.Errorf("webhook target event = %q, want run.held", e.Event)
	}
	select {
	case subject := <-emailer.sent:
		if subject != "SwitchTender run run_held is waiting for approval" {
			t.Errorf("email target subject = %q, want the held subject", subject)
		}
	default:
		t.Error("the email target was not told about the hold")
	}

	// Test 1: the failure-only webhook, the pager, the dashboard, and the text target hear nothing.
	noneArrive(t, failOnly, "the failure-only webhook")
	noneArrive(t, pages, "the pager target")
	noneArrive(t, annotations, "the dashboard annotation target")
	noneArrive(t, texts, "the text target")

	// Test 2: a failure reaches each of those, so the silences above could have been broken.
	failed := r.Clone()
	failed.Status = run.StatusFailed
	d.notifyRunTargets(failed)
	d.notifyWG.Wait()
	if e := nextEvent(t, failOnly); e.Event != "run.finished" {
		t.Errorf("failure-only webhook event = %q, want run.finished", e.Event)
	}
	for name, ch := range map[string]chan struct{}{
		"the pager target": pages, "the dashboard annotation target": annotations,
		"the text target": texts,
	} {
		select {
		case <-ch:
		default:
			t.Errorf("%s was not told about a failure, so its silence above proves nothing", name)
		}
	}
}

// TestEmailOnFailureOnlyLeavesAHeldRunUnsent pins that the operator's failures-only email setting
// covers holds too. A hold is not a failure, so an install that asked for failure email alone does
// not start receiving approval requests it never configured.
func TestEmailOnFailureOnlyLeavesAHeldRunUnsent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name          string
		OnFailureOnly bool
		WantSubject   string
	}{{ // Test 0: an install that emails every run is told about the hold.
		Name: "every run", OnFailureOnly: false,
		WantSubject: "SwitchTender run run_held is waiting for approval",
	}, { // Test 1: an install that emails failures only is not.
		Name: "failures only", OnFailureOnly: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			emailer := &captureEmailer{sent: make(chan string, 4)}
			d := New(run.NewMemStore(), okRunner(), nil, WithEmail(emailer, test.OnFailureOnly))
			defer d.Close()
			d.notifyHeld(heldRun())
			d.notifyWG.Wait()
			var got string
			select {
			case got = <-emailer.sent:
			default:
			}
			if diff := cmp.Diff(test.WantSubject, got); diff != "" {
				t.Errorf("email subject mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// lateKeyStore answers the first idempotency lookup with nothing. That is the store a submission
// sees when a concurrent submission carrying the same key saves after this one looked and before
// this one saves.
type lateKeyStore struct {
	// Store answers every call but the one missed lookup.
	run.Store
	// missed records that the one miss has been served.
	missed atomic.Bool
}

// ByIdempotencyKey reports no run the first time it is asked, then answers from the store.
func (s *lateKeyStore) ByIdempotencyKey(ctx context.Context, key string) (*run.Run, error) {
	if s.missed.CompareAndSwap(false, true) {
		return nil, run.ErrNotFound
	}
	return s.Store.ByIdempotencyKey(ctx, key)
}

// TestARacedSubmissionDoesNotAnnounceTheWinnersHold covers the other way one request arrives
// twice. A retry that lands after the first submission is answered by the lookup before anything is
// saved, but two that race both pass the lookup, and the loser's save is refused on the key and
// handed the winner. The winner's hold was announced when it was held, so the loser must not
// announce it again.
func TestARacedSubmissionDoesNotAnnounceTheWinnersHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hookURL, events := captureEvents(t)
	store := &lateKeyStore{Store: run.NewMemStore()}
	winner := heldRun()
	winner.IdempotencyKey = "raced"
	if err := store.Save(ctx, winner); err != nil {
		t.Fatalf("Save(winner) error = %v", err)
	}
	d := New(store, okRunner(), nil, WithPolicies(holdEverything(t)),
		WithWebhooks([]string{hookURL}), WithNotifyClient(http.DefaultClient))
	defer d.Close()

	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("echo hi"),
		run.WithIdempotencyKey("raced"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.ID != winner.ID {
		t.Fatalf("the raced submission returned %s, want the winner %s", got.ID, winner.ID)
	}
	noneArrive(t, events, "the losing submission of a raced request")
}

// TestAHeldSplitAndAHeldRetryAreAnnouncedForTheParentAlone pins the two fan-out paths. Their shards
// are stored held beside the parent, and the approver releases the parent, so the hold is one
// question: announced once for the parent and never once per shard.
func TestAHeldSplitAndAHeldRetryAreAnnouncedForTheParentAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Test 0: a split a rule holds is announced once, for its parent.
	hookURL, events := captureEvents(t)
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, nil,
		WithPolicies(holdEverything(t)), WithWebhooks([]string{hookURL}), WithNoJanitor(),
		WithNotifyClient(http.DefaultClient))
	defer d.Close()
	parent, err := d.SubmitSplit(ctx, "site.yml", "hosts.ini", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("split is %s, want it held", parent.Status)
	}
	if e := nextEvent(t, events); e.Event != "run.held" || e.Run.ID != parent.ID {
		t.Errorf("webhook event = %+v, want run.held for the split %s", e, parent.ID)
	}
	noneArrive(t, events, "an announcement for a held shard")

	// Test 1: a retry of the failed shards held for approval is announced once, for the retry.
	retryURL, retryEvents := captureEvents(t)
	retryStore := run.NewMemStore()
	flaky := &flakyRunnerLister{hosts: []string{"a", "b", "c", "d"}, failHost: "b"}
	rd := New(retryStore, flaky, nil, WithWebhooks([]string{retryURL}),
		WithNotifyClient(http.DefaultClient))
	defer rd.Close()
	failed, err := rd.SubmitSplit(ctx, "play.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if got := waitTerminal(t, retryStore, failed.ID); got.Status != run.StatusFailed {
		t.Fatalf("split status = %q, want failed", got.Status)
	}
	if e := nextEvent(t, retryEvents); e.Event != "run.finished" || e.Run.ID != failed.ID {
		t.Errorf("webhook event = %+v, want run.finished for the failed split", e)
	}
	flaky.fixed.Store(true)
	retry, err := rd.RetryFailedShards(ctx, failed.ID, run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	if retry.Status != run.StatusPendingApproval {
		t.Fatalf("retry is %s, want it held", retry.Status)
	}
	if e := nextEvent(t, retryEvents); e.Event != "run.held" || e.Run.ID != retry.ID {
		t.Errorf("webhook event = %+v, want run.held for the retry %s", e, retry.ID)
	}
	noneArrive(t, retryEvents, "an announcement for a held retry shard")
}

// TestAnApplyThePlanGateHoldsIsAnnounced covers the hold nobody submitted by hand. The plan gate
// plans a terraform apply, finds it over the destroy limit, and proposes an apply that waits for a
// person, so that apply has to be announced with the count that held it, the same as any other.
func TestAnApplyThePlanGateHoldsIsAnnounced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hookURL, events := captureEvents(t)
	policies := policy.NewMemStore()
	guard := policy.NewPolicy("tf-destroy-guard")
	guard.Tool, guard.MaxDestroy = run.ToolTerraform, 0
	if err := policies.Save(ctx, guard); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}
	store := run.NewMemStore()
	d := New(store, &planGateRunner{summary: "Plan: 0 to add, 0 to change, 3 to destroy.\n"}, nil,
		WithPolicies(policies), WithWebhooks([]string{hookURL}), WithNotifyClient(http.DefaultClient))
	defer d.Close()

	plan, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand("infra/legacy"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	// The plan run finishes as the apply is proposed, and the two deliveries race, so the held
	// announcement is looked for among both rather than expected first.
	var held heldEvent
	for range 2 {
		if e := nextEvent(t, events); e.Event == "run.held" {
			held = e
		}
	}
	if held.Run.ID == "" || held.Run.ID == plan.ID {
		t.Fatalf("held event = %+v, want run.held for the apply the plan proposed", held)
	}
	apply, err := store.Get(ctx, held.Run.ID)
	if err != nil {
		t.Fatalf("Get(apply) error = %v", err)
	}
	const wantHeld = "tf-destroy-guard (plan destroys 3, limit 0)"
	if apply.ProposedFrom != plan.ID || apply.HeldByPolicy != wantHeld {
		t.Errorf("announced run = proposed from %q held by %q, want the plan's apply held by %q",
			apply.ProposedFrom, apply.HeldByPolicy, wantHeld)
	}
}

// TestAHoldReachesEveryChannelFinishedRunsReach pins the channel list a hold is announced on. The
// message tests call each formatter directly, and the delivery tests wire only a webhook and Slack,
// so a channel dropped from the hold's list failed nothing: its formatter still passed and nothing
// was listening for it. Each channel here is the only one configured, so its row fails by name.
func TestAHoldReachesEveryChannelFinishedRunsReach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name is the channel.
		Name string
		// Option configures that channel alone at url.
		Option func(url string) Option
	}{{ // Test 0: A webhook.
		Name: "webhook", Option: func(u string) Option { return WithWebhooks([]string{u}) },
	}, { // Test 1: Slack.
		Name: "slack", Option: func(u string) Option { return WithSlack([]string{u}) },
	}, { // Test 2: Mattermost.
		Name: "mattermost", Option: func(u string) Option { return WithMattermost([]string{u}) },
	}, { // Test 3: Rocket.Chat.
		Name: "rocketchat", Option: func(u string) Option { return WithRocketChat([]string{u}) },
	}, { // Test 4: Discord.
		Name: "discord", Option: func(u string) Option { return WithDiscord([]string{u}) },
	}, { // Test 5: Microsoft Teams.
		Name: "teams", Option: func(u string) Option { return WithTeams([]string{u}) },
	}, { // Test 6: ntfy.
		Name: "ntfy", Option: func(u string) Option { return WithNtfy([]string{u}, "") },
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			url, hits := captureHits(t)
			d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor(),
				WithNotifyClient(http.DefaultClient), test.Option(url))
			defer d.Close()

			d.notifyHeld(heldRun())
			d.notifyWG.Wait()
			select {
			case <-hits:
			default:
				t.Fatalf("%s was not told about the hold", test.Name)
			}
			noneArrive(t, hits, "a second announcement on "+test.Name)
		})
	}
}

// TestAHoldNeverPagesTextsOrAnnotates pins the other half of where a hold goes. A pager, a text
// message, and a dashboard annotation report incidents, and a hold is a question, so none of them
// hears it. The pager and the text channel send only failures in any case, but a dashboard
// annotates every run it is given, so it is what shows a hold wired to the wrong list. Each is then
// shown a failure through the same wiring, so a silence above is the code deciding and not a
// channel that could never have fired.
func TestAHoldNeverPagesTextsOrAnnotates(t *testing.T) {
	t.Parallel()
	pagerURL, pages := captureHits(t)
	twilioURL, texts := captureHits(t)
	grafanaURL, annotations := captureHits(t)
	d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor(),
		WithNotifyClient(http.DefaultClient), WithPagerDuty([]string{"team-rk"}),
		WithTwilio("AC0000", "twilio-token", "+15550000000", []string{"+15551111111"}),
		WithGrafana([]string{grafanaURL}, "grafana-token"))
	d.pagerDutyEndpoint = pagerURL
	d.twilioBaseURL = twilioURL
	defer d.Close()

	// Test 0: the hold reaches none of them.
	d.notifyHeld(heldRun())
	d.notifyWG.Wait()
	noneArrive(t, pages, "the pager")
	noneArrive(t, texts, "the text channel")
	noneArrive(t, annotations, "the dashboard")

	// Test 1: a failure reaches all three, so the wiring above could have fired.
	failed := heldRun()
	failed.Status = run.StatusFailed
	d.notify(failed)
	d.notifyWG.Wait()
	for name, ch := range map[string]chan struct{}{
		"the pager": pages, "the text channel": texts, "the dashboard": annotations,
	} {
		select {
		case <-ch:
		default:
			t.Errorf("%s was not told about a failure, so its silence above proves nothing", name)
		}
	}
}
