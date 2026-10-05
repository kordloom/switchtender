package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
)

// skipNoticeRun returns the notice the scheduler announces for a skipped fire.
func skipNoticeRun() *run.Run {
	return &run.Run{
		Kind: run.KindSkippedFire, Source: "schedule", SourceID: "sch_nightly",
		TemplateID: "tpl_patch", InventoryID: "inv_window", Warning: "no hosts matched",
		CreatedAt: time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC), Actor: "system:scheduler",
		Labels: map[string]string{"schedule": "nightly patch", "inventory": "patch window"},
	}
}

// TestSkippedFireMessagesNeverReadAsFailures pins what each channel says about a skipped fire.
// Every formatter read any status other than succeeded as a failure, so without its own branch a
// skip would have arrived as a red failure for a schedule that did nothing wrong.
func TestSkippedFireMessagesNeverReadAsFailures(t *testing.T) {
	t.Parallel()
	r := skipNoticeRun()
	card := teamsCardPayload(r).Attachments[0].Content.Body[0]
	tests := []struct {
		Got      any
		WantText any
	}{{ // Test 0: Slack.
		Got: slackMessage(r),
		WantText: ":fast_forward: SwitchTender schedule *nightly patch* skipped: no hosts matched. " +
			"Its inventory \"patch window\" resolved to no hosts, so no run was started.",
	}, { // Test 1: Discord.
		Got: discordMessage(r),
		WantText: "SwitchTender schedule **nightly patch** skipped: no hosts matched. Its inventory " +
			"\"patch window\" resolved to no hosts, so no run was started.",
	}, { // Test 2: The ntfy title.
		Got:      ntfyHeaders(r)["Title"],
		WantText: "SwitchTender schedule nightly patch skipped: no hosts matched.",
	}, { // Test 3: A skip is not raised to high priority on ntfy.
		Got: ntfyHeaders(r)["Priority"], WantText: "",
	}, { // Test 4: The ntfy body.
		Got:      ntfyBody(r),
		WantText: "Its inventory \"patch window\" resolved to no hosts, so no run was started.",
	}, { // Test 5: The email subject.
		Got:      emailSubject(r),
		WantText: "SwitchTender schedule nightly patch skipped: no hosts matched.",
	}, { // Test 6: The webhook event.
		Got: webhookEvent(r), WantText: "schedule.skipped",
	}, { // Test 7: The Teams title.
		Got: card["text"], WantText: "SwitchTender schedule nightly patch skipped: no hosts matched.",
	}, { // Test 8: The Teams color is a warning, never the failure color.
		Got: card["color"], WantText: "Warning",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantText, test.Got); diff != "" {
				t.Errorf("message mismatch (-want +got):\n%s", diff)
			}
			if text, ok := test.Got.(string); ok && strings.Contains(strings.ToLower(text), "fail") {
				t.Errorf("a skip reads as a failure: %q", text)
			}
		})
	}
	body := emailBody(r)
	for _, want := range []string{"Inventory: patch window", "No run was started",
		"/ui/inventories?preview=inv_window"} {
		if !strings.Contains(body, want) {
			t.Errorf("email body does not say %q:\n%s", want, body)
		}
	}
}

// fixedRouter is a NotificationRouter that answers every run with the same targets and counts how
// often it was asked.
type fixedRouter struct {
	// targets is what every run is routed to.
	targets []run.NotifyTarget
	// mu guards asked.
	mu sync.Mutex
	// asked counts the runs the router was asked about.
	asked int
}

// Targets counts the question and returns the fixed targets.
func (f *fixedRouter) Targets(context.Context, *run.Run) []run.NotifyTarget {
	f.mu.Lock()
	f.asked++
	f.mu.Unlock()
	return f.targets
}

// questions returns how many runs the router was asked about.
func (f *fixedRouter) questions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// skipEvent is the part of a skipped fire's webhook payload these tests read.
type skipEvent struct {
	// Event is schedule.skipped.
	Event string `json:"event"`
	// Run is the notice: what skipped and why.
	Run struct {
		// ID is empty, since no run was started.
		ID string `json:"id"`
		// Kind marks the notice of a skipped fire.
		Kind string `json:"kind"`
		// SourceID names the schedule.
		SourceID string `json:"source_id"`
		// InventoryID names the inventory that matched nothing.
		InventoryID string `json:"inventory_id"`
		// Labels carry the schedule's and the inventory's names.
		Labels map[string]string `json:"labels"`
	} `json:"run"`
}

// TestAnnouncedSkipTakesTheNamedPathAndSkipsPagers pins where a skipped fire goes: through the
// named-target path a run takes, to a webhook and an email recipient, and never to a pager, a
// dashboard annotation, or a text message, the rule a hold follows. The same targets are then shown
// a failed run, so each silence is the code deciding and not a target that could never have fired.
func TestAnnouncedSkipTakesTheNamedPathAndSkipsPagers(t *testing.T) {
	t.Parallel()
	hookURL := make(chan skipEvent, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e skipEvent
		if err := json.NewDecoder(r.Body).Decode(&e); err == nil {
			hookURL <- e
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	pagerURL, pages := captureHits(t)
	grafanaURL, annotations := captureHits(t)
	twilioURL, texts := captureHits(t)
	emailer := &captureEmailer{sent: make(chan string, 4)}
	router := &fixedRouter{targets: []run.NotifyTarget{
		{Kind: run.NotifyWebhook, URL: srv.URL},
		{Kind: run.NotifyPagerDuty, Key: "team-rk"},
		{Kind: run.NotifyGrafana, URL: grafanaURL, Key: "grafana-token"},
		{Kind: run.NotifyTwilio, To: "+15552222222"},
		{Kind: run.NotifyEmail, To: "ops@example.com"},
	}}
	invs := inventory.NewMemStore()
	if err := invs.Save(context.Background(), &inventory.Inventory{ID: "inv_window",
		Name: "patch window", Kind: inventory.KindSmart, HostFilter: "name=web1",
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save(inventory) error = %v", err)
	}
	d := New(run.NewMemStore(), okRunner(), nil, WithNotifyClient(http.DefaultClient),
		WithNotificationRouter(router), WithInventories(invs), WithEmail(emailer, false),
		WithTwilio("AC0000", "twilio-token", "+15550000000", nil))
	d.pagerDutyEndpoint = pagerURL
	d.twilioBaseURL = twilioURL
	defer d.Close()

	notice := skipNoticeRun()
	delete(notice.Labels, "inventory")
	d.AnnounceSkip(notice)
	d.notifyWG.Wait()

	// Test 0: the webhook hears the skip, with the inventory named from the store.
	select {
	case e := <-hookURL:
		if e.Event != "schedule.skipped" || e.Run.ID != "" || e.Run.Kind != run.KindSkippedFire ||
			e.Run.SourceID != "sch_nightly" || e.Run.InventoryID != "inv_window" ||
			e.Run.Labels["inventory"] != "patch window" {
			t.Errorf("webhook payload = %+v, want the skipped fire of sch_nightly over its inventory",
				e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook target was not told about the skip")
	}
	// Test 1: the email recipient hears it.
	select {
	case subject := <-emailer.sent:
		if subject != "SwitchTender schedule nightly patch skipped: no hosts matched." {
			t.Errorf("email subject = %q, want the skipped subject", subject)
		}
	default:
		t.Error("the email target was not told about the skip")
	}
	// Test 2: the pager, the dashboard, and the text target hear nothing.
	noneArrive(t, pages, "the pager target")
	noneArrive(t, annotations, "the dashboard annotation target")
	noneArrive(t, texts, "the text target")
	// The notice the scheduler built is left as it was.
	if _, ok := notice.Labels["inventory"]; ok {
		t.Error("announcing the skip wrote the inventory name onto the caller's notice")
	}

	// Test 3: a run that is not a skip is not announced as one.
	asked := router.questions()
	d.AnnounceSkip(&run.Run{ID: "run_x", Status: run.StatusFailed})
	d.notifyWG.Wait()
	if router.questions() != asked {
		t.Error("AnnounceSkip routed a run that is not a skipped fire")
	}

	// Test 4: a failure reaches each silent target, so the silences above could have been broken.
	d.notifyNamed(&run.Run{ID: "run_failed", Status: run.StatusFailed, Playbook: "patch.yml"})
	d.notifyWG.Wait()
	for name, ch := range map[string]chan struct{}{
		"the pager target": pages, "the dashboard annotation target": annotations,
		"the text target": texts,
	} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Errorf("%s was not told about a failure, so its silence above proves nothing", name)
		}
	}
}
