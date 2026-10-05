package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// notifyHookSecret is a webhook address whose path is the credential, distinctive enough to prove
// it never reaches a preview.
const notifyHookSecret = "https://hooks.example.com/services/NOTIFY_HOOK_SECRET"

// notifyExport is an awxkit-shaped export with a notification template of every type, attached to a
// job template, a workflow, an organization, and a project the way awxkit writes attachments.
const notifyExport = `{
 "notification_templates": [
  {"name": "ops hook", "organization": {"name": "Default", "type": "organization"},
   "notification_type": "webhook", "description": "pages ops",
   "notification_configuration": {"url": "` + notifyHookSecret + `", "headers": {},
     "http_method": "POST", "username": "", "password": "", "disable_ssl_verification": false}},
  {"name": "ops slack", "organization": {"name": "Default", "type": "organization"},
   "notification_type": "slack",
   "notification_configuration": {"token": "$encrypted$", "channels": ["#ops"], "hex_color": ""}},
  {"name": "ops pager", "notification_type": "pagerduty",
   "notification_configuration": {"token": "$encrypted$", "subdomain": "acme",
     "service_key": "R0UTING", "client_name": "awx"}},
  {"name": "ops grafana", "notification_type": "grafana",
   "notification_configuration": {"grafana_url": "https://grafana.example.com",
     "grafana_key": "$encrypted$"}},
  {"name": "ops mail", "notification_type": "email",
   "notification_configuration": {"host": "smtp.example.com", "port": 25, "username": "",
     "password": "", "sender": "awx@example.com", "recipients": ["a@example.com", "b@example.com"],
     "use_tls": false, "use_ssl": false, "timeout": 30}},
  {"name": "ops sms", "notification_type": "twilio",
   "notification_configuration": {"account_sid": "AC123", "account_token": "$encrypted$",
     "from_number": "+15550000000", "to_numbers": ["+15551111111", "+15552222222"]}},
  {"name": "ops irc", "notification_type": "irc",
   "notification_configuration": {"server": "irc.example.com", "port": 6667, "nickname": "awx",
     "password": "$encrypted$", "use_ssl": false, "targets": ["#ops"]}},
  {"name": "ops chat", "notification_type": "mattermost",
   "notification_configuration": {"mattermost_url": "https://mm.example.com/hooks/abc",
     "mattermost_username": "awx", "mattermost_channel": "", "mattermost_icon_url": "",
     "mattermost_no_verify_ssl": false}}
 ],
 "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://example.com/infra.git",
   "related": {"notification_templates_error": [
     {"name": "ops mail", "type": "notification_template"}]}}],
 "job_templates": [
  {"name": "deploy", "playbook": "deploy.yml", "project": "infra",
   "organization": {"name": "Default", "type": "organization"},
   "related": {
    "notification_templates_started": [{"name": "ops hook", "type": "notification_template",
      "organization": {"name": "Default", "type": "organization"}}],
    "notification_templates_success": [{"name": "ops slack", "type": "notification_template",
      "organization": {"name": "Default", "type": "organization"}}],
    "notification_templates_error": [{"name": "ops pager", "type": "notification_template"},
      {"name": "ops sms", "type": "notification_template"},
      {"name": "ops irc", "type": "notification_template"}],
    "schedules": [{"name": "nightly check", "rrule": "DTSTART:20260101T020000Z RRULE:FREQ=DAILY",
      "job_type": "check"}]}},
  {"name": "rotate", "playbook": "rotate.yml", "project": "infra",
   "organization": {"name": "Other", "type": "organization"}}
 ],
 "workflow_job_templates": [
  {"name": "rollout", "organization": {"name": "Default", "type": "organization"},
   "related": {
    "notification_templates_approvals": [{"name": "ops mail", "type": "notification_template"}],
    "workflow_nodes": [{"id": 1, "identifier": "deploy",
      "unified_job_template": {"name": "deploy", "type": "job_template"}}]}}
 ],
 "organizations": [
  {"name": "Default", "related": {
    "notification_templates_error": [{"name": "ops chat", "type": "notification_template"}]}}
 ]
}`

// planFor imports the notification export at the fixed import time.
func planFor(t *testing.T) *Plan {
	t.Helper()
	plan, err := FromAWX([]byte(notifyExport), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	return plan
}

// templateID returns the id of the imported template with the given name.
func templateID(t *testing.T, plan *Plan, name string) string {
	t.Helper()
	for _, tpl := range plan.Templates {
		if tpl.Name == name {
			return tpl.ID
		}
	}
	t.Fatalf("no template named %q in %d templates", name, len(plan.Templates))
	return ""
}

// TestAWXNotificationTemplatesComeAcross pins how each AWX notification type becomes a target: the
// address and recipient carried, a secret AWX exports only as "$encrypted$" left for somebody to
// enter, a Twilio template with two numbers becoming two targets, and IRC refused.
func TestAWXNotificationTemplatesComeAcross(t *testing.T) {
	t.Parallel()
	plan := planFor(t)
	type shape struct {
		// Name, Kind, To, URL and NeedsSecret are what the target is, with its URL masked.
		Name, Kind, To, URL string
		NeedsSecret         bool
	}
	var got []shape
	for _, n := range plan.Notifications {
		got = append(got, shape{n.Name, n.Kind, n.To, n.URLHint, n.NeedsSecret})
	}
	want := []shape{
		{Name: "ops hook", Kind: "webhook", URL: "https://hooks.example.com/…"},
		{Name: "ops slack", Kind: "slack", NeedsSecret: true},
		{Name: "ops pager", Kind: "pagerduty"},
		{Name: "ops grafana", Kind: "grafana", URL: "https://grafana.example.com/…", NeedsSecret: true},
		{Name: "ops mail", Kind: "email", To: "a@example.com, b@example.com"},
		{Name: "ops sms (+15551111111)", Kind: "twilio", To: "+15551111111"},
		{Name: "ops sms (+15552222222)", Kind: "twilio", To: "+15552222222"},
		{Name: "ops chat", Kind: "mattermost", URL: "https://mm.example.com/…"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("targets mismatch (-want +got):\n%s", diff)
	}
	report := plan.Report()
	leftOut := strings.Join(report.LeftOut, "\n")
	for _, wantLine := range []string{`"ops irc" posts to IRC`, `project "infra" has notification`} {
		if !strings.Contains(leftOut, wantLine) {
			t.Errorf("left out does not name %q:\n%s", wantLine, leftOut)
		}
	}
	review := strings.Join(report.NeedsReview, "\n")
	for _, wantLine := range []string{`"ops slack" posts to Slack with a bot token, to #ops`,
		`"ops grafana" needs its secret entered`, `"ops mail" sends through the SMTP server`,
		`organization "Default" has notification templates attached`} {
		if !strings.Contains(review, wantLine) {
			t.Errorf("needs review does not name %q:\n%s", wantLine, review)
		}
	}
	if strings.Contains(leftOut, "notification template, which") ||
		strings.Contains(leftOut, "notification templates, which") {
		t.Errorf("notification templates are still counted as not imported:\n%s", leftOut)
	}
}

// TestAWXNotificationAttachmentsComeAcross pins the attachments: each AWX event list becomes the
// matching event, a target that did not come across is named, a workflow's approval list attaches
// for approvals, an organization's own list attaches to the organization once rather than to each
// of its templates, and the copy an overriding schedule fires is attached alongside its template.
func TestAWXNotificationAttachmentsComeAcross(t *testing.T) {
	t.Parallel()
	plan := planFor(t)
	names := map[string]string{}
	for _, tpl := range plan.Templates {
		names[tpl.ID] = tpl.Name
	}
	for _, o := range plan.Orgs {
		names[o.ID] = o.Name
	}
	targets := map[string]string{}
	for _, n := range plan.Notifications {
		targets[n.ID] = n.Name
	}
	var got []string
	for _, a := range plan.Attachments {
		got = append(got, fmt.Sprintf("%s on %s %s for %s", targets[a.NotificationID], a.ObjectKind,
			names[a.ObjectID], a.Event))
	}
	sort.Strings(got)
	copyName := "deploy (nightly check)"
	var want []string
	for _, object := range []string{"deploy", copyName} {
		want = append(want,
			"ops hook on template "+object+" for started",
			"ops slack on template "+object+" for success",
			"ops pager on template "+object+" for failure",
			"ops sms (+15551111111) on template "+object+" for failure",
			"ops sms (+15552222222) on template "+object+" for failure",
		)
	}
	want = append(want, "ops mail on template rollout for approval",
		"ops chat on org Default for failure")
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("attachments mismatch (-want +got):\n%s", diff)
	}
	if _, ok := warningContaining(t, plan.Warnings, `template "deploy" is attached in AWX to `+
		`notification template "ops irc"`); !ok {
		t.Errorf("the attachment to a refused target was not named.\nwarnings: %v", plan.Warnings)
	}
	for _, a := range plan.Attachments {
		if a.ObjectID == templateID(t, plan, "rotate") {
			t.Errorf("a template from another organization was attached: %+v", a)
		}
	}
}

// TestAWXNotificationPreviewCarriesNoSecret pins that a preview, which returns the plan, never
// holds a target's address: it is held unexported until an apply seals it.
func TestAWXNotificationPreviewCarriesNoSecret(t *testing.T) {
	t.Parallel()
	plan := planFor(t)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, secret := range []string{"NOTIFY_HOOK_SECRET", "R0UTING", "hooks/abc"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the plan a preview returns carries %q", secret)
		}
	}
	if strings.Contains(strings.Join(plan.Warnings, "\n"), "NOTIFY_HOOK_SECRET") {
		t.Error("a warning carries the webhook address")
	}
}

// applyStores returns in-memory stores for an apply.
func applyStores(sealer *credential.Sealer) ApplyStores {
	return ApplyStores{
		Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
		Sources: invsource.NewMemStore(), Credentials: credential.NewMemStore(),
		Templates: template.NewMemStore(), Schedules: schedule.NewMemStore(),
		Notifications: notification.NewMemStore(), Sealer: sealer,
	}
}

// TestAWXNotificationApplySealsTargets pins the apply: with a key, each target is stored sealed and
// opens to the address the export held, and every attachment is stored; without one, a target that
// carries a secret is stored waiting for it rather than in the clear.
func TestAWXNotificationApplySealsTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("import-test-passphrase", "import-test-salt")
	tests := []struct {
		Sealer          *credential.Sealer
		WantHookSecret  bool
		WantHookOpened  string
		WantAttachments int
	}{{ // Test 0: With a key the address is sealed and opens again.
		Sealer: sealer, WantHookOpened: notifyHookSecret, WantAttachments: 14,
	}, { // Test 1: Without one the target waits for its secret.
		Sealer: nil, WantHookSecret: true, WantAttachments: 14,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan := planFor(t)
			stores := applyStores(test.Sealer)
			if _, err := plan.Apply(ctx, stores); err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			stored, err := stores.Notifications.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var hook *notification.Notification
			for _, n := range stored {
				if strings.Contains(n.SealedURL+n.SealedKey+n.URLHint, "NOTIFY_HOOK_SECRET") {
					t.Errorf("target %q is stored in the clear", n.Name)
				}
				if n.Name == "ops hook" {
					hook = n
				}
			}
			if hook == nil {
				t.Fatal("the webhook target was not stored")
			}
			if hook.NeedsSecret != test.WantHookSecret {
				t.Errorf("NeedsSecret = %v, want %v", hook.NeedsSecret, test.WantHookSecret)
			}
			if test.WantHookOpened != "" {
				opened, err := hook.Target(test.Sealer)
				if err != nil || opened.URL != test.WantHookOpened {
					t.Errorf("Target() = %+v, %v, want the export's address", opened, err)
				}
			}
			total := 0
			for _, n := range stored {
				list, err := stores.Notifications.Attachments(ctx, n.ID)
				if err != nil {
					t.Fatalf("Attachments() error = %v", err)
				}
				total += len(list)
			}
			if total != test.WantAttachments {
				t.Errorf("stored attachments = %d, want %d", total, test.WantAttachments)
			}
			if _, err := hook.Target(test.Sealer); test.WantHookSecret &&
				!errors.Is(err, notification.ErrNeedsSecret) {
				t.Errorf("a waiting target opened: %v", err)
			}
		})
	}
	plan := planFor(t)
	_, err := plan.Apply(ctx, ApplyStores{Projects: project.NewMemStore(),
		Inventories: inventory.NewMemStore(), Sources: invsource.NewMemStore(),
		Credentials: credential.NewMemStore(), Templates: template.NewMemStore(),
		Schedules: schedule.NewMemStore()})
	if err == nil || !strings.Contains(err.Error(), "notification targets not enabled") {
		t.Errorf("Apply() with no notification store = %v, want a refusal before writing", err)
	}
}
