package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// flakySchedules is a schedule store whose reads fail a set number of times before they work, or
// every time when the count is negative, standing in for a database that drops a connection.
type flakySchedules struct {
	schedule.Store
	// mu guards failReads.
	mu sync.Mutex
	// failReads is how many reads fail before they work, or every one when negative.
	failReads int
}

// Get fails while reads are set to fail, then reads the wrapped store.
func (f *flakySchedules) Get(ctx context.Context, id string) (*schedule.Schedule, error) {
	f.mu.Lock()
	fail := f.failReads != 0
	if f.failReads > 0 {
		f.failReads--
	}
	f.mu.Unlock()
	if fail {
		return nil, errors.New("connection reset by peer")
	}
	return f.Store.Get(ctx, id)
}

// lineageFixture is a store with a target on a template, a schedule that fires the template, and
// a target on the run's project, which every lookup reaches.
func lineageFixture(t *testing.T) (Store, schedule.Store) {
	t.Helper()
	store := NewMemStore()
	mustTarget(t, store, "ntf_tpl", run.NotifyTarget{Kind: run.NotifyWebhook,
		URL: "https://hooks.example.com/tpl"})
	mustAttach(t, store, "ntf_tpl", KindTemplate, "tpl_deploy", EventFailure)
	mustTarget(t, store, "ntf_owner", run.NotifyTarget{Kind: run.NotifyWebhook,
		URL: "https://hooks.example.com/owner"})
	mustAttach(t, store, "ntf_owner", KindOrg, "org_owner", EventFailure)
	mustTarget(t, store, "ntf_proj", run.NotifyTarget{Kind: run.NotifyWebhook,
		URL: "https://hooks.example.com/proj"})
	mustAttach(t, store, "ntf_proj", KindProject, "proj_infra", EventFailure)
	schedules := schedule.NewMemStore()
	if err := schedules.Save(context.Background(), &schedule.Schedule{ID: "sch_nightly",
		Cron: "0 2 * * *", TemplateID: "tpl_deploy", Enabled: true,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save schedule error = %v", err)
	}
	return store, schedules
}

// TestRouterRecipientsFailOnATemplateItCannotRead pins that routing treats a template it cannot
// read as a failure rather than as a template with no targets. A read of the schedule behind the
// run, or of the organization that owns its template, that fails is an error, so the event is
// recorded again once the store answers, while the best-effort path still tells the targets it
// could find. A template that no longer exists is different: it is passed over without an error,
// since asking again finds nothing more, and the router logs which run lost its template's targets.
func TestRouterRecipientsFailOnATemplateItCannotRead(t *testing.T) {
	t.Parallel()
	gone := fmt.Errorf("%w: template tpl_deploy no longer exists", ErrTemplateGone)
	tests := []struct {
		OwnerErr        error
		Name            string
		SourceID        string
		FailReads       int
		WantErr         bool
		WantRecipients  []string
		WantTargets     int
		WantGoneWarning bool
	}{{ // Test 0: Every read answers, so the template and its owner are told.
		Name: "readable", SourceID: "sch_nightly",
		WantRecipients: []string{"ntf_proj", "ntf_tpl", "ntf_owner"}, WantTargets: 3,
	}, { // Test 1: The schedule behind the run cannot be read.
		Name: "schedule unreadable", SourceID: "sch_nightly", FailReads: -1, WantErr: true,
		WantTargets: 1,
	}, { // Test 2: The schedule behind the run no longer exists.
		Name: "schedule gone", SourceID: "sch_deleted",
		WantRecipients: []string{"ntf_proj"}, WantTargets: 1, WantGoneWarning: true,
	}, { // Test 3: The template's owner cannot be read.
		Name: "owner unreadable", SourceID: "sch_nightly",
		OwnerErr: errors.New("connection reset by peer"), WantErr: true, WantTargets: 2,
	}, { // Test 4: The template no longer exists when its owner is looked up.
		Name: "template gone", SourceID: "sch_nightly", OwnerErr: gone,
		WantRecipients: []string{"ntf_proj", "ntf_tpl"}, WantTargets: 2, WantGoneWarning: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, schedules := lineageFixture(t)
			flaky := &flakySchedules{Store: schedules, failReads: test.FailReads}
			core, logs := observer.New(zap.WarnLevel)
			router := NewRouter(store, testSealer{}, SourceLineage(flaky, nil, nil), zap.New(core),
				WithTemplateOrgs(func(context.Context, string) (string, error) {
					if test.OwnerErr != nil {
						return "", test.OwnerErr
					}
					return "org_owner", nil
				}))
			in := &run.Run{ID: "run_routed", Status: run.StatusFailed, Source: "schedule",
				SourceID: test.SourceID, ProjectID: "proj_infra"}
			got, err := router.Recipients(ctx, in)
			if (err != nil) != test.WantErr {
				t.Fatalf("Recipients() error = %v, want error %v", err, test.WantErr)
			}
			var ids []string
			for _, rc := range got {
				ids = append(ids, rc.NotificationID)
			}
			if diff := cmp.Diff(test.WantRecipients, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Recipients() mismatch (-want +got):\n%s", diff)
			}
			flaky.mu.Lock()
			flaky.failReads = test.FailReads
			flaky.mu.Unlock()
			if n := len(router.Targets(ctx, in)); n != test.WantTargets {
				t.Errorf("Targets() = %d targets, want %d", n, test.WantTargets)
			}
			warned := false
			for _, entry := range logs.All() {
				if strings.Contains(entry.Message, "no longer exists") &&
					entry.ContextMap()["run_id"] == "run_routed" {
					warned = true
				}
			}
			if warned != test.WantGoneWarning {
				t.Errorf("logged the template that is gone = %v, want %v: %v", warned,
					test.WantGoneWarning, logs.All())
			}
		})
	}
}

// TestOutboxRecordWaitsForATemplateItCannotReadYet pins the recording path end to end: an event
// whose template cannot be read for a moment is asked again within the record bound and recorded
// for the template's targets, instead of being recorded at once for the others alone and counted as
// told.
func TestOutboxRecordWaitsForATemplateItCannotReadYet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, schedules := lineageFixture(t)
	flaky := &flakySchedules{Store: schedules, failReads: 2}
	o := NewOutbox(store, NewRouter(store, testSealer{}, SourceLineage(flaky, nil, nil), nil),
		testSealer{}, nil)
	in := &run.Run{ID: "run_waits", Status: run.StatusFailed, Source: "schedule",
		SourceID: "sch_nightly", ProjectID: "proj_infra"}
	if err := o.Record(ctx, in, Branch{}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	list, err := store.Deliveries(ctx, DeliveryFilter{RunID: "run_waits"})
	if err != nil {
		t.Fatalf("Deliveries() error = %v", err)
	}
	var got []string
	for _, d := range list {
		got = append(got, d.NotificationID)
	}
	if diff := cmp.Diff([]string{"ntf_proj", "ntf_tpl"}, got, cmpopts.SortSlices(func(a,
		b string) bool {
		return a < b
	})); diff != "" {
		t.Errorf("recorded targets mismatch (-want +got):\n%s", diff)
	}
}
