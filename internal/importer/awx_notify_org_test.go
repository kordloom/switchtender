package importer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// orgNotifyExport is an export whose organization "Ops" has a notification template attached to
// itself, with two job templates in it, one in another organization, and a second organization
// with notifications and no template that came across.
const orgNotifyExport = `{
 "notification_templates": [
  {"name": "ops chat", "organization": {"name": "Ops", "type": "organization"},
   "notification_type": "mattermost",
   "notification_configuration": {"mattermost_url": "https://mm.example.com/hooks/ops"}}
 ],
 "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://example.com/infra.git"}],
 "job_templates": [
  {"name": "deploy", "playbook": "deploy.yml", "project": "infra",
   "organization": {"name": "Ops", "type": "organization"}},
  {"name": "patch", "playbook": "patch.yml", "project": "infra",
   "organization": {"name": "Ops", "type": "organization"}},
  {"name": "report", "playbook": "report.yml", "project": "infra",
   "organization": {"name": "Finance", "type": "organization"}}
 ],
 "organizations": [
  {"name": "Ops", "related": {
    "notification_templates_error": [{"name": "ops chat", "type": "notification_template"}],
    "notification_templates_success": [{"name": "ops chat", "type": "notification_template"}]}},
  {"name": "Empty", "related": {
    "notification_templates_error": [{"name": "ops chat", "type": "notification_template"}]}}
 ]
}`

// orgApplyStores returns in-memory stores for an apply, with an organization store holding the
// named organizations, or none when orgs is nil.
func orgApplyStores(t *testing.T, orgs []string) ApplyStores {
	t.Helper()
	s := ApplyStores{
		Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
		Sources: invsource.NewMemStore(), Credentials: credential.NewMemStore(),
		Templates: template.NewMemStore(), Schedules: schedule.NewMemStore(),
		Notifications: notification.NewMemStore(),
		Sealer:        credential.NewSealer("org-test-passphrase", "org-test-salt"),
	}
	if orgs == nil {
		return s
	}
	s.Orgs = org.NewMemStore()
	for i, name := range orgs {
		if err := s.Orgs.Save(context.Background(), &org.Org{ID: fmt.Sprintf("org_held%d", i),
			Name: name, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Save(org) error = %v", err)
		}
	}
	return s
}

// TestAWXOrganizationNotificationsLandOnTheOrganization pins decision 37: an AWX organization's own
// notification attachments go on the organization when one resolves, imported from the same export
// or matched to the one organization this install holds under that name, with its templates placed
// in it; an ambiguous name falls back to the organization's templates rather than guessing, so does
// an install with no organization store, and an organization with no template to fall back to is
// unresolved. The report states which, and no attachment is ever made both ways.
func TestAWXOrganizationNotificationsLandOnTheOrganization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		Held           []string
		WantOutcome    OrgNotification
		WantEmpty      string
		WantOrgs       []string
		WantAttached   []string
		WantTemplateOn string
	}{{ // Test 0: Nothing of that name is held, so the organization is imported.
		Held: []string{"Platform"},
		WantOutcome: OrgNotification{Organization: "Ops", Outcome: OrgNotifyImported,
			Attachments: 2},
		WantEmpty: OrgNotifyImported,
		WantOrgs:  []string{"Empty", "Ops", "Platform"},
		WantAttached: []string{"failure on org Empty", "failure on org Ops",
			"success on org Ops"},
		WantTemplateOn: "Ops",
	}, { // Test 1: One organization of that name is held, so it is matched rather than duplicated.
		Held: []string{"Ops"},
		WantOutcome: OrgNotification{Organization: "Ops", Outcome: OrgNotifyMatched,
			OrgID: "org_held0", Attachments: 2},
		WantEmpty: OrgNotifyImported,
		WantOrgs:  []string{"Empty", "Ops"},
		WantAttached: []string{"failure on org Empty", "failure on org Ops",
			"success on org Ops"},
		WantTemplateOn: "Ops",
	}, { // Test 2: Two of that name are held, so it falls back to the templates, not a guess.
		Held: []string{"Ops", "Ops"},
		WantOutcome: OrgNotification{Organization: "Ops", Outcome: OrgNotifyFellBack, Templates: 2,
			Attachments: 4, Reason: `2 organizations here are named "Ops" and the import does ` +
				"not guess between them"},
		WantEmpty: OrgNotifyImported,
		WantOrgs:  []string{"Empty", "Ops", "Ops"},
		WantAttached: []string{"failure on org Empty", "failure on template deploy",
			"failure on template patch", "success on template deploy", "success on template patch"},
	}, { // Test 3: No organization store, so nothing can be created or matched.
		WantOutcome: OrgNotification{Organization: "Ops", Outcome: OrgNotifyFellBack, Templates: 2,
			Attachments: 4, Reason: "organizations are not enabled on this install"},
		WantEmpty: OrgNotifyUnresolved,
		WantAttached: []string{"failure on template deploy", "failure on template patch",
			"success on template deploy", "success on template patch"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(orgNotifyExport), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			stores := orgApplyStores(t, test.Held)
			if _, err := plan.Apply(ctx, stores); err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			report := plan.Report()
			outcomes := map[string]OrgNotification{}
			for _, o := range report.Organizations {
				outcomes[o.Organization] = o
			}
			ignoreNew := cmpopts.IgnoreFields(OrgNotification{}, "OrgID")
			if test.WantOutcome.Outcome != OrgNotifyImported {
				ignoreNew = cmpopts.IgnoreFields(OrgNotification{})
			}
			if diff := cmp.Diff(test.WantOutcome, outcomes["Ops"], ignoreNew); diff != "" {
				t.Errorf("outcome mismatch (-want +got):\n%s", diff)
			}
			if got := outcomes["Empty"].Outcome; got != test.WantEmpty {
				t.Errorf("Empty's outcome = %q, want %q", got, test.WantEmpty)
			}
			orgNames := map[string]string{}
			var gotOrgs []string
			if stores.Orgs != nil {
				held, err := stores.Orgs.List(ctx)
				if err != nil {
					t.Fatalf("List(orgs) error = %v", err)
				}
				for _, o := range held {
					orgNames[o.ID] = o.Name
					gotOrgs = append(gotOrgs, o.Name)
				}
			}
			sort.Strings(gotOrgs)
			if diff := cmp.Diff(test.WantOrgs, gotOrgs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("organizations mismatch (-want +got):\n%s", diff)
			}
			templates, err := stores.Templates.List(ctx)
			if err != nil {
				t.Fatalf("List(templates) error = %v", err)
			}
			names := map[string]string{}
			for _, tpl := range templates {
				names[tpl.ID] = tpl.Name
				placed := orgNames[tpl.OrgID]
				switch tpl.Name {
				case "deploy", "patch":
					if placed != test.WantTemplateOn {
						t.Errorf("template %s is in %q, want %q", tpl.Name, placed,
							test.WantTemplateOn)
					}
				default:
					if tpl.OrgID != "" {
						t.Errorf("template %s from another organization was placed in %q",
							tpl.Name, placed)
					}
				}
			}
			targets, err := stores.Notifications.List(ctx)
			if err != nil || len(targets) != 1 {
				t.Fatalf("List(targets) = %v, %v, want the one target", targets, err)
			}
			attached, err := stores.Notifications.Attachments(ctx, targets[0].ID)
			if err != nil {
				t.Fatalf("Attachments() error = %v", err)
			}
			var got []string
			for _, a := range attached {
				name := names[a.ObjectID]
				if a.ObjectKind == notification.KindOrg {
					name = orgNames[a.ObjectID]
				}
				got = append(got, a.Event+" on "+a.ObjectKind+" "+name)
			}
			sort.Strings(got)
			if diff := cmp.Diff(test.WantAttached, got); diff != "" {
				t.Errorf("attachments mismatch (-want +got):\n%s", diff)
			}
			review := strings.Join(report.NeedsReview, "\n")
			if !strings.Contains(review, `organization "Ops" has notification templates `+
				"attached in AWX: "+test.WantOutcome.Outcome) {
				t.Errorf("the report does not state %q for Ops:\n%s", test.WantOutcome.Outcome,
					review)
			}
			if test.WantEmpty == OrgNotifyUnresolved && !strings.Contains(
				strings.Join(report.LeftOut, "\n"),
				`organization "Empty" has notification templates attached in AWX: unresolved`) {
				t.Errorf("the report does not state Empty as unresolved:\n%s",
					strings.Join(report.LeftOut, "\n"))
			}
		})
	}
}

// TestAWXOrganizationNotificationsMatchByAWXID pins that an organization is found by its AWX id
// when the templates name it that way, the shape the REST API writes, as well as by its name.
func TestAWXOrganizationNotificationsMatchByAWXID(t *testing.T) {
	t.Parallel()
	export := strings.NewReplacer(
		`"organization": {"name": "Ops", "type": "organization"}},`, `"organization": 7},`,
		`{"name": "Ops", "related"`, `{"id": 7, "name": "Ops", "related"`,
	).Replace(orgNotifyExport)
	plan, err := FromAWX([]byte(export), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	stores := orgApplyStores(t, []string{})
	if _, err := plan.Apply(context.Background(), stores); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	opsID := ""
	for _, o := range plan.Orgs {
		if o.Name == "Ops" {
			opsID = o.ID
		}
	}
	placed := 0
	for _, tpl := range plan.Templates {
		if opsID != "" && tpl.OrgID == opsID {
			placed++
		}
	}
	if placed != 2 {
		t.Errorf("%d templates placed in the organization, want the 2 that name it by id", placed)
	}
}

// TestAWXNeedsSecretTargetsKeepWhatTheyHave pins decision 38 at the import: a target missing only
// its secret keeps every part the export did carry, sealed, and names only what is missing.
func TestAWXNeedsSecretTargetsKeepWhatTheyHave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	export := `{"notification_templates": [
  {"name": "dash", "notification_type": "grafana", "description": "dashboards",
   "notification_configuration": {"grafana_url": "https://grafana.example.com",
     "grafana_key": "$encrypted$"}},
  {"name": "hook", "notification_type": "webhook",
   "notification_configuration": {"url": "$encrypted$", "headers": {}}},
  {"name": "pager", "notification_type": "pagerduty",
   "notification_configuration": {"service_key": "$encrypted$", "subdomain": "acme"}},
  {"name": "slack", "notification_type": "slack",
   "notification_configuration": {"token": "$encrypted$", "channels": ["#ops"]}}]}`
	plan, err := FromAWX([]byte(export), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	sealer := credential.NewSealer("needs-secret-passphrase", "needs-secret-salt")
	stores := orgApplyStores(t, nil)
	stores.Sealer = sealer
	if _, err := plan.Apply(ctx, stores); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	stored, err := stores.Notifications.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	type shape struct {
		// Name, Description, Kind, and Missing are what the target is and still needs.
		Name, Description, Kind string
		Missing                 []string
		// Known is what opening the stored parts gives back.
		Known run.NotifyTarget
	}
	var got []shape
	for _, n := range stored {
		if !n.NeedsSecret {
			t.Errorf("target %s is not waiting for its secret", n.Name)
		}
		known, err := n.Known(sealer)
		if err != nil {
			t.Fatalf("Known(%s) error = %v", n.Name, err)
		}
		got = append(got, shape{n.Name, n.Description, n.Kind, n.Missing(), known})
	}
	want := []shape{
		{Name: "dash", Description: "dashboards", Kind: "grafana", Missing: []string{"key"},
			Known: run.NotifyTarget{Kind: "grafana", URL: "https://grafana.example.com"}},
		{Name: "hook", Kind: "webhook", Missing: []string{"url"},
			Known: run.NotifyTarget{Kind: "webhook"}},
		{Name: "pager", Kind: "pagerduty", Missing: []string{"key"},
			Known: run.NotifyTarget{Kind: "pagerduty"}},
		{Name: "slack", Kind: "slack", Missing: []string{"url"},
			Known: run.NotifyTarget{Kind: "slack"}},
	}
	byName := func(a, b shape) bool { return a.Name < b.Name }
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(byName)); diff != "" {
		t.Errorf("stored targets mismatch (-want +got):\n%s", diff)
	}
}
