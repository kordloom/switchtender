package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/trigger"
)

// testSealer seals by prefixing, so a test can tell a sealed value from a plain one.
type testSealer struct {
	// off makes the sealer report no key.
	off bool
}

// Enabled reports whether the sealer has a key.
func (s testSealer) Enabled() bool { return !s.off }

// Seal prefixes the plaintext.
func (s testSealer) Seal(plain string) (string, error) { return "sealed:" + plain, nil }

// Open strips the prefix, refusing a value it did not seal.
func (s testSealer) Open(sealed string) (string, error) {
	plain, ok := strings.CutPrefix(sealed, "sealed:")
	if !ok {
		return "", errors.New("not sealed")
	}
	return plain, nil
}

// mustTarget stores a target configured with t and returns its id.
func mustTarget(t *testing.T, store Store, id string, cfg run.NotifyTarget) string {
	t.Helper()
	n := &Notification{ID: id, Name: id, CreatedAt: time.Now()}
	if err := n.SetTarget(cfg, testSealer{}); err != nil {
		t.Fatalf("SetTarget(%s) error = %v", id, err)
	}
	if err := store.Save(context.Background(), n); err != nil {
		t.Fatalf("Save(%s) error = %v", id, err)
	}
	return id
}

// mustAttach attaches a target to an object for an event.
func mustAttach(t *testing.T, store Store, ntf, kind, object, event string) {
	t.Helper()
	if err := store.Attach(context.Background(), &Attachment{
		ID: NewAttachmentID(), NotificationID: ntf, ObjectKind: kind, ObjectID: object,
		Event: event, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
}

// TestRouterFansOutAcrossAttachments pins the AWX behavior a migration relies on: a target attached
// to the template, the schedule, the project, or the organization a run came from hears that run's
// events, each target once however many of them it is attached through, and only for the events
// it was attached for.
func TestRouterFansOutAcrossAttachments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	tplHook := mustTarget(t, store, "ntf_tpl", run.NotifyTarget{Kind: run.NotifyWebhook,
		URL: "https://hooks.example.com/tpl"})
	schedChat := mustTarget(t, store, "ntf_sched", run.NotifyTarget{Kind: run.NotifySlack,
		URL: "https://hooks.slack.com/services/T/B/sched"})
	projPager := mustTarget(t, store, "ntf_proj", run.NotifyTarget{Kind: run.NotifyPagerDuty,
		Key: "routing-key"})
	orgMail := mustTarget(t, store, "ntf_org", run.NotifyTarget{Kind: run.NotifyEmail,
		To: "ops@example.com"})
	shell := "ntf_shell"
	if err := store.Save(ctx, &Notification{ID: shell, Name: "imported", Kind: run.NotifySlack,
		NeedsSecret: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	mustAttach(t, store, tplHook, KindTemplate, "tpl_deploy", EventStarted)
	mustAttach(t, store, tplHook, KindTemplate, "tpl_deploy", EventFailure)
	mustAttach(t, store, schedChat, KindSchedule, "sch_nightly", EventSuccess)
	mustAttach(t, store, schedChat, KindSchedule, "sch_nightly", EventFailure)
	mustAttach(t, store, projPager, KindProject, "proj_infra", EventFailure)
	mustAttach(t, store, projPager, KindProject, "proj_infra", EventAttention)
	mustAttach(t, store, orgMail, KindOrg, "org_ops", EventApproval)
	mustAttach(t, store, orgMail, KindOrg, "org_ops", EventFailure)
	// Attached at the template and the organization both, and told once.
	mustAttach(t, store, tplHook, KindOrg, "org_ops", EventFailure)
	mustAttach(t, store, shell, KindTemplate, "tpl_deploy", EventFailure)
	// Attached to another template, so never told about this run.
	other := mustTarget(t, store, "ntf_other", run.NotifyTarget{Kind: run.NotifyWebhook,
		URL: "https://hooks.example.com/other"})
	mustAttach(t, store, other, KindTemplate, "tpl_other", EventFailure)

	schedules := schedule.NewMemStore()
	if err := schedules.Save(ctx, &schedule.Schedule{ID: "sch_nightly", Cron: "0 2 * * *",
		TemplateID: "tpl_deploy", Enabled: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save schedule error = %v", err)
	}
	router := NewRouter(store, testSealer{}, SourceLineage(schedules, nil, nil), nil)
	fired := func(status run.Status) *run.Run {
		return &run.Run{ID: "run_1", Status: status, Source: "schedule", SourceID: "sch_nightly",
			ProjectID: "proj_infra", OrgID: "org_ops"}
	}
	tests := []struct {
		In          *run.Run
		WantTargets []run.NotifyTarget
	}{{ // Test 0: A start reaches only what is attached for starts, through the template.
		In: fired(run.StatusRunning),
		WantTargets: []run.NotifyTarget{
			{Kind: run.NotifyWebhook, URL: "https://hooks.example.com/tpl"},
		},
	}, { // Test 1: A hold reaches the organization's approval target.
		In:          fired(run.StatusPendingApproval),
		WantTargets: []run.NotifyTarget{{Kind: run.NotifyEmail, To: "ops@example.com"}},
	}, { // Test 2: A success reaches the schedule's chat target.
		In: fired(run.StatusSucceeded),
		WantTargets: []run.NotifyTarget{
			{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/T/B/sched"},
		},
	}, { // Test 3: A failure reaches all four, each once, and not the shell or the other template.
		In: fired(run.StatusFailed),
		WantTargets: []run.NotifyTarget{
			{Kind: run.NotifyWebhook, URL: "https://hooks.example.com/tpl"},
			{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/T/B/sched"},
			{Kind: run.NotifyPagerDuty, Key: "routing-key"},
			{Kind: run.NotifyEmail, To: "ops@example.com"},
		},
	}, { // Test 4: A cancel is a failure in AWX's sense and is announced as one.
		In: fired(run.StatusCanceled),
		WantTargets: []run.NotifyTarget{
			{Kind: run.NotifyWebhook, URL: "https://hooks.example.com/tpl"},
			{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/T/B/sched"},
			{Kind: run.NotifyPagerDuty, Key: "routing-key"},
			{Kind: run.NotifyEmail, To: "ops@example.com"},
		},
	}, { // Test 5: A pending run is at no event.
		In: fired(run.StatusPending),
	}, { // Test 6: A launch by hand reaches the template through its source.
		In: &run.Run{ID: "run_2", Status: run.StatusRunning, Source: "template",
			SourceID: "tpl_deploy"},
		WantTargets: []run.NotifyTarget{
			{Kind: run.NotifyWebhook, URL: "https://hooks.example.com/tpl"},
		},
	}, { // Test 7: An attention alert reaches only what is attached for alerts, whatever the run's
		// status says.
		In: func() *run.Run {
			r := fired(run.StatusPending)
			r.Attention = &run.AttentionNote{ID: "att_1", Blocker: "no_worker"}
			return r
		}(),
		WantTargets: []run.NotifyTarget{{Kind: run.NotifyPagerDuty, Key: "routing-key"}},
	}}
	byKind := func(a, b run.NotifyTarget) bool { return a.Kind < b.Kind }
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := router.Targets(ctx, test.In)
			if diff := cmp.Diff(test.WantTargets, got, cmpopts.EquateEmpty(),
				cmpopts.SortSlices(byKind)); diff != "" {
				t.Errorf("Targets() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSourceLineage pins how a run is traced to its template: directly from a launch, through the
// schedule or trigger that fired it, and through a rerun to the run it reran.
func TestSourceLineage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	schedules := schedule.NewMemStore()
	if err := schedules.Save(ctx, &schedule.Schedule{ID: "sch_1", Cron: "0 2 * * *",
		TemplateID: "tpl_s", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	triggers := trigger.NewMemStore()
	if err := triggers.Save(ctx, &trigger.Trigger{ID: "trg_1", TemplateID: "tpl_t",
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runs := run.NewMemStore()
	if err := runs.Save(ctx, &run.Run{ID: "run_origin", Source: "trigger", SourceID: "trg_1",
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	lineage := SourceLineage(schedules, triggers, runs)
	tests := []struct {
		In           *run.Run
		WantTemplate string
	}{
		{In: &run.Run{Source: "template", SourceID: "tpl_a"}, WantTemplate: "tpl_a"},   // Test 0.
		{In: &run.Run{Source: "schedule", SourceID: "sch_1"}, WantTemplate: "tpl_s"},   // Test 1.
		{In: &run.Run{Source: "trigger", SourceID: "trg_1"}, WantTemplate: "tpl_t"},    // Test 2.
		{In: &run.Run{Source: "rerun", SourceID: "run_origin"}, WantTemplate: "tpl_t"}, // Test 3.
		{In: &run.Run{Source: "schedule", SourceID: "sch_gone"}},                       // Test 4.
		{In: &run.Run{Source: "api"}},                                                  // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantTemplate, lineage.TemplateOf(ctx, test.In)); diff != "" {
				t.Errorf("TemplateOf() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSetTargetSealsAndNeverKeepsThePlaintext pins the secret handling: the address and key are
// stored sealed, the readable form is a masked hint, and a target that needs sealing is refused
// when there is no key rather than stored in the clear.
func TestSetTargetSealsAndNeverKeepsThePlaintext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Sealer   Sealer
		Want     error
		In       run.NotifyTarget
		WantHint string
	}{{ // Test 0: A webhook address is sealed and hinted.
		Sealer: testSealer{}, In: run.NotifyTarget{Kind: run.NotifySlack,
			URL: "https://hooks.slack.com/services/T0/B1/secret"},
		WantHint: "https://hooks.slack.com/…",
	}, { // Test 1: A secret with no key to seal it is refused.
		Sealer: testSealer{off: true}, In: run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "rk"},
		Want: ErrSealing,
	}, { // Test 2: The same with no sealer at all.
		In:   run.NotifyTarget{Kind: run.NotifyWebhook, URL: "https://h.example.com/x"},
		Want: ErrSealing,
	}, { // Test 3: A recipient-only target needs no key.
		Sealer: testSealer{off: true}, In: run.NotifyTarget{Kind: run.NotifyEmail, To: "a@b.c"},
	}, { // Test 4: A target missing what its kind needs is refused before anything is sealed.
		Sealer: testSealer{}, In: run.NotifyTarget{Kind: run.NotifyGrafana,
			URL: "https://grafana.example.com"},
		Want: errAny,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			n := &Notification{ID: "ntf_x"}
			err := n.SetTarget(test.In, test.Sealer)
			switch {
			case test.Want == errAny:
				if err == nil {
					t.Fatal("SetTarget() error = nil, want a refusal")
				}
				return
			case !errors.Is(err, test.Want):
				t.Fatalf("SetTarget() error = %v, want %v", err, test.Want)
			case err != nil:
				return
			}
			for _, field := range []string{n.SealedURL, n.SealedKey, n.URLHint, n.To} {
				if test.In.URL != "" && strings.Contains(field, test.In.URL) &&
					!strings.HasPrefix(field, "sealed:") {
					t.Errorf("a field holds the address in the clear: %q", field)
				}
			}
			if n.URLHint != test.WantHint {
				t.Errorf("URLHint = %q, want %q", n.URLHint, test.WantHint)
			}
			got, err := n.Target(test.Sealer)
			if err != nil {
				t.Fatalf("Target() error = %v", err)
			}
			if diff := cmp.Diff(test.In, got); diff != "" {
				t.Errorf("Target() did not open what SetTarget sealed (-want +got):\n%s", diff)
			}
		})
	}
}

// errAny marks a table entry that expects some refusal without naming which.
var errAny = errors.New("any error")

// TestNormalize pins the event and object names an attachment accepts, AWX's among them.
func TestNormalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want      error
		In        string
		WantEvent string
		WantKind  string
	}{
		{In: "error", WantEvent: EventFailure, Want: nil},       // Test 0: AWX's name for failure.
		{In: "approvals", WantEvent: EventApproval, Want: nil},  // Test 1: AWX's plural.
		{In: "Started", WantEvent: EventStarted, Want: nil},     // Test 2: Case does not matter.
		{In: "finished", Want: ErrEvent},                        // Test 3: Not an event.
		{In: "workflow", WantKind: KindTemplate, Want: nil},     // Test 4: A workflow is a template.
		{In: "organization", WantKind: KindOrg, Want: nil},      // Test 5: Either spelling.
		{In: "inventory", Want: ErrObject},                      // Test 6: Not attachable.
		{In: "skipped", WantEvent: EventSkipped, Want: nil},     // Test 7: A skipped schedule fire.
		{In: "attention", WantEvent: EventAttention, Want: nil}, // Test 8: An attention alert.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if test.WantKind != "" || errors.Is(test.Want, ErrObject) {
				got, err := NormalizeObjectKind(test.In)
				if !errors.Is(err, test.Want) || got != test.WantKind {
					t.Errorf("NormalizeObjectKind(%q) = %q, %v", test.In, got, err)
				}
				return
			}
			got, err := NormalizeEvent(test.In)
			if !errors.Is(err, test.Want) || got != test.WantEvent {
				t.Errorf("NormalizeEvent(%q) = %q, %v", test.In, got, err)
			}
		})
	}
}

// TestRouterReachesTheTemplatesOrganization pins that a target attached to an organization hears
// the runs of every template the organization owns, whoever launched them, as AWX tells an
// organization's notification templates about every job its templates run, and still hears a run
// stamped with the organization, each target once.
func TestRouterReachesTheTemplatesOrganization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	mustTarget(t, store, "ntf_org", run.NotifyTarget{Kind: run.NotifyEmail, To: "ops@example.com"})
	mustAttach(t, store, "ntf_org", KindOrg, "org_ops", EventFailure)
	owners := map[string]string{"tpl_ops": "org_ops", "tpl_other": "org_other"}
	router := NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil,
		WithTemplateOrgs(func(_ context.Context, id string) string { return owners[id] }))
	without := NewRouter(store, testSealer{}, SourceLineage(nil, nil, nil), nil)
	tests := []struct {
		In          *run.Run
		WantTargets int
		WantPlain   int
	}{{ // Test 0: Launched by somebody outside the organization, run stamped with no org.
		In: &run.Run{ID: "run_1", Status: run.StatusFailed, Source: "template",
			SourceID: "tpl_ops"},
		WantTargets: 1,
	}, { // Test 1: Stamped with the organization and of its template, told once.
		In: &run.Run{ID: "run_2", Status: run.StatusFailed, Source: "template",
			SourceID: "tpl_ops", OrgID: "org_ops"},
		WantTargets: 1, WantPlain: 1,
	}, { // Test 2: A template another organization owns.
		In: &run.Run{ID: "run_3", Status: run.StatusFailed, Source: "template",
			SourceID: "tpl_other"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := len(router.Targets(ctx, test.In)); got != test.WantTargets {
				t.Errorf("Targets() = %d targets, want %d", got, test.WantTargets)
			}
			recipients, err := router.Recipients(ctx, test.In)
			if err != nil || len(recipients) != test.WantTargets {
				t.Errorf("Recipients() = %d recipients, %v, want %d", len(recipients), err,
					test.WantTargets)
			}
			if got := len(without.Targets(ctx, test.In)); got != test.WantPlain {
				t.Errorf("Targets() without template organizations = %d, want %d", got,
					test.WantPlain)
			}
		})
	}
}
