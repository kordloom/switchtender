package notification

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// TestSkippedFireReachesSkipAndScheduleFailureTargets pins who hears a schedule's skipped fire: a
// target attached for skipped to the schedule, its template, or its organization, and a target
// attached to the schedule itself for failure, since an AWX import never names skipped. A target
// attached only for started, success, or approval does not, and neither does a failure target on
// anything but the schedule.
func TestSkippedFireReachesSkipAndScheduleFailureTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	hook := func(id string) string {
		return mustTarget(t, store, id, run.NotifyTarget{Kind: run.NotifyWebhook,
			URL: "https://hooks.example.com/" + id})
	}
	for _, a := range []struct{ id, kind, object, event string }{
		{"ntf_sched_skip", KindSchedule, "sch_nightly", EventSkipped},
		{"ntf_sched_fail", KindSchedule, "sch_nightly", EventFailure},
		{"ntf_sched_start", KindSchedule, "sch_nightly", EventStarted},
		{"ntf_sched_ok", KindSchedule, "sch_nightly", EventSuccess},
		{"ntf_sched_held", KindSchedule, "sch_nightly", EventApproval},
		{"ntf_tpl_skip", KindTemplate, "tpl_deploy", EventSkipped},
		{"ntf_tpl_fail", KindTemplate, "tpl_deploy", EventFailure},
		{"ntf_org_skip", KindOrg, "org_ops", EventSkipped},
		{"ntf_org_fail", KindOrg, "org_ops", EventFailure},
		{"ntf_other_skip", KindSchedule, "sch_other", EventSkipped},
	} {
		mustAttach(t, store, hook(a.id), a.kind, a.object, a.event)
	}
	schedules := schedule.NewMemStore()
	if err := schedules.Save(ctx, &schedule.Schedule{ID: "sch_nightly", Cron: "0 2 * * *",
		TemplateID: "tpl_deploy", Enabled: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save schedule error = %v", err)
	}
	router := NewRouter(store, testSealer{}, SourceLineage(schedules, nil, nil), nil)

	tests := []struct {
		In       *run.Run
		WantURLs []string
	}{{ // Test 0: A skipped fire reaches the skip targets and the schedule's failure target.
		In: &run.Run{Kind: run.KindSkippedFire, Source: "schedule", SourceID: "sch_nightly",
			OrgID: "org_ops"},
		WantURLs: []string{
			"https://hooks.example.com/ntf_org_skip",
			"https://hooks.example.com/ntf_sched_fail",
			"https://hooks.example.com/ntf_sched_skip",
			"https://hooks.example.com/ntf_tpl_skip",
		},
	}, { // Test 1: A failed run reaches the failure targets and no skip target.
		In: &run.Run{ID: "run_1", Status: run.StatusFailed, Source: "schedule",
			SourceID: "sch_nightly", OrgID: "org_ops"},
		WantURLs: []string{
			"https://hooks.example.com/ntf_org_fail",
			"https://hooks.example.com/ntf_sched_fail",
			"https://hooks.example.com/ntf_tpl_fail",
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var urls []string
			for _, target := range router.Targets(ctx, test.In) {
				urls = append(urls, target.URL)
			}
			if diff := cmp.Diff(test.WantURLs, urls, cmpopts.EquateEmpty(),
				cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Errorf("Targets() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEventOfSkippedFire pins that the notice of a skipped fire is at the skipped event whatever
// its status says, and that no run is read as one.
func TestEventOfSkippedFire(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In        *run.Run
		WantEvent string
	}{{ // Test 0: The notice of a skipped fire.
		In: &run.Run{Kind: run.KindSkippedFire}, WantEvent: EventSkipped,
	}, { // Test 1: A failed run is a failure.
		In: &run.Run{Status: run.StatusFailed}, WantEvent: EventFailure,
	}, { // Test 2: A pending run is at no event.
		In: &run.Run{Status: run.StatusPending}, WantEvent: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := EventOf(test.In); got != test.WantEvent {
				t.Errorf("EventOf() = %q, want %q", got, test.WantEvent)
			}
		})
	}
}
