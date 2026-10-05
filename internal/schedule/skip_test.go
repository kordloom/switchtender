package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// errSubmitter answers every submission with err, standing in for a dispatcher whose composed
// inventory resolved to no hosts, or one that failed for an ordinary reason. A nil err starts a
// run.
type errSubmitter struct {
	// err is what every submission returns.
	err error
	// mu guards calls.
	mu sync.Mutex
	// calls counts submissions.
	calls int
}

// answer records a submission and returns the configured outcome.
func (e *errSubmitter) answer() (*run.Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	return &run.Run{ID: fmt.Sprintf("run_%d", e.calls)}, nil
}

// Submit answers with the configured outcome.
func (e *errSubmitter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run, error) {
	return e.answer()
}

// SubmitSplit answers with the configured outcome.
func (e *errSubmitter) SubmitSplit(context.Context, string, string, int, ...run.SubmitOption) (*run.Run, error) {
	return e.answer()
}

// SubmitPipeline answers with the configured outcome.
func (e *errSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep,
	...run.SubmitOption) (*run.Run, error) {
	return e.answer()
}

// skipRecorder keeps every notice a scheduler announces.
type skipRecorder struct {
	// mu guards got.
	mu sync.Mutex
	// got holds the notices in the order they were announced.
	got []*run.Run
}

// AnnounceSkip records the notice.
func (r *skipRecorder) AnnounceSkip(n *run.Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, n)
}

// notices returns what was announced.
func (r *skipRecorder) notices() []*run.Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*run.Run(nil), r.got...)
}

// skipRefusingAudits takes every entry except a skip's, standing in for a chain that fails between
// a fire's entry and its skip's.
type skipRefusingAudits struct {
	audit.Store
}

// Append refuses a skip entry and keeps every other.
func (s skipRefusingAudits) Append(ctx context.Context, e *audit.Entry) error {
	if strings.HasSuffix(e.Path, "/skipped") {
		return errors.New("chain unavailable")
	}
	return s.Store.Append(ctx, e)
}

// seedSkipSchedule stores a due schedule that fires a template over a composed inventory, and the
// template, so a tick fires it.
func seedSkipSchedule(t *testing.T, store Store, templates template.Store) {
	t.Helper()
	ctx := context.Background()
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_web", Name: "patch web", Playbook: "patch.yml", InventoryID: "inv_smart",
		OrgID: "org_ops", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save(template) error = %v", err)
	}
	due := time.Now().Add(-time.Minute)
	if err := store.Save(ctx, &Schedule{
		ID: "sch_skip", Name: "nightly patch", Cron: "* * * * *", TemplateID: "tpl_web",
		Enabled: true, CreatedAt: time.Now(), NextRunAt: &due,
	}); err != nil {
		t.Fatalf("Save(schedule) error = %v", err)
	}
}

// makeDue moves a schedule's next fire into the past, so the next tick fires it again.
func makeDue(t *testing.T, store Store, id string) {
	t.Helper()
	ctx := context.Background()
	sc, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	due := time.Now().Add(-time.Minute)
	sc.NextRunAt = &due
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// TestFireThatMatchesNoHostsIsSkippedNotFailed pins what a fire whose composed inventory resolved
// to no hosts leaves behind: a skip on the schedule rather than a failure, its own chain entry
// after the fire's, and a notice to the schedule's targets. An ordinary failure is still a failure,
// and a skip the chain will not take is recorded as one, so a skip never exists without its
// evidence.
func TestFireThatMatchesNoHostsIsSkippedNotFailed(t *testing.T) {
	t.Parallel()
	noHosts := fmt.Errorf("%w: patch window matched nothing", inventory.ErrNoHosts)
	tests := []struct {
		SubmitErr       error
		RefuseSkip      bool
		WantSkip        string
		WantSkipped     int
		WantErrorPart   string
		WantPaths       []string
		WantNoticeCount int
	}{{ // Test 0: No hosts matched, so the fire is skipped, on the chain, and announced.
		SubmitErr: noHosts, WantSkip: SkipNoHosts, WantSkipped: 1,
		WantPaths:       []string{"/schedules/sch_skip/fired", "/schedules/sch_skip/skipped"},
		WantNoticeCount: 1,
	}, { // Test 1: An ordinary failure stays a failure and is never announced as a skip.
		SubmitErr: errors.New("dispatcher unavailable"), WantErrorPart: "dispatcher unavailable",
		WantPaths: []string{"/schedules/sch_skip/fired"},
	}, { // Test 2: A skip the chain refuses is recorded as a failed fire, not a quiet skip.
		SubmitErr: noHosts, RefuseSkip: true, WantErrorPart: "could not be recorded",
		WantPaths: []string{"/schedules/sch_skip/fired"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, templates := NewMemStore(), template.NewMemStore()
			seedSkipSchedule(t, store, templates)
			chain := audit.NewMemStore()
			audits := audit.Store(chain)
			if test.RefuseSkip {
				audits = skipRefusingAudits{Store: chain}
			}
			notices := &skipRecorder{}
			s := NewScheduler(store, &errSubmitter{err: test.SubmitErr}, zap.NewNop(),
				WithTemplates(templates), WithAudits(audits), WithSkipNotifier(notices))
			now := time.Now()
			s.tick(now)

			got, err := store.Get(ctx, "sch_skip")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.LastSkip != test.WantSkip || got.SkippedFires != test.WantSkipped {
				t.Errorf("skip = %q x%d, want %q x%d", got.LastSkip, got.SkippedFires,
					test.WantSkip, test.WantSkipped)
			}
			if !strings.Contains(got.LastError, test.WantErrorPart) ||
				(test.WantErrorPart == "" && got.LastError != "") {
				t.Errorf("LastError = %q, want it to say %q", got.LastError, test.WantErrorPart)
			}
			if got.LastRunAt == nil || !got.LastRunAt.Equal(now) {
				t.Errorf("LastRunAt = %v, want the fire time %v", got.LastRunAt, now)
			}
			entries, err := chain.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			var paths []string
			for _, e := range entries {
				paths = append(paths, e.Path)
			}
			if diff := cmp.Diff(test.WantPaths, paths, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("chain paths mismatch (-want +got):\n%s", diff)
			}
			if ok, at := audit.Verify(entries); !ok {
				t.Errorf("the chain does not verify, broken at %d", at)
			}
			if n := len(notices.notices()); n != test.WantNoticeCount {
				t.Errorf("announced %d notices, want %d", n, test.WantNoticeCount)
			}
		})
	}
}

// TestSkipEntryCommitsWhatWasSkipped proves the skip's chain entry is a scheduler entry whose
// content digest opens to the schedule, its template and inventory, the fire time, and the reason,
// so an auditor holding the record can check it against the chain.
func TestSkipEntryCommitsWhatWasSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, templates := NewMemStore(), template.NewMemStore()
	seedSkipSchedule(t, store, templates)
	chain := audit.NewMemStore()
	s := NewScheduler(store, &errSubmitter{err: inventory.ErrNoHosts}, zap.NewNop(),
		WithTemplates(templates), WithAudits(chain))
	now := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	sc, err := store.Get(ctx, "sch_skip")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if err := s.recordSkipEntry(ctx, sc, "inv_smart", now); err != nil {
		t.Fatalf("recordSkipEntry() error = %v", err)
	}
	entries, err := chain.Chain(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("Chain() = %d entries, %v, want the one skip entry", len(entries), err)
	}
	e := entries[0]
	if e.Method != audit.MethodSchedule || e.Actor != "system:scheduler" || e.ActorType != "system" {
		t.Errorf("entry = %s by %s (%s), want SCHEDULE by system:scheduler (system)", e.Method,
			e.Actor, e.ActorType)
	}
	body, err := json.Marshal(skipRecord{
		ScheduleID: "sch_skip", Name: "nightly patch", TemplateID: "tpl_web",
		InventoryID: "inv_smart", FiredAt: "2026-10-01T02:00:00Z", Reason: SkipNoHosts,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !audit.VerifyContentDigest(e.ContentDigest, e.Nonce, body) {
		t.Errorf("the skip entry's digest does not open to the skip record %s", body)
	}
}

// TestSkipsCountInARowAndAFireThatRunsResetsThem pins the count behind the badge: each skipped fire
// adds one, and the first fire that starts a run clears the count and the reason.
func TestSkipsCountInARowAndAFireThatRunsResetsThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, templates := NewMemStore(), template.NewMemStore()
	seedSkipSchedule(t, store, templates)
	sub := &errSubmitter{err: inventory.ErrNoHosts}
	notices := &skipRecorder{}
	s := NewScheduler(store, sub, zap.NewNop(), WithTemplates(templates),
		WithAudits(audit.NewMemStore()), WithSkipNotifier(notices))
	for range SkipBadgeFires {
		s.tick(time.Now())
		makeDue(t, store, "sch_skip")
	}
	got, err := store.Get(ctx, "sch_skip")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.SkippedFires != SkipBadgeFires || got.LastSkip != SkipNoHosts {
		t.Errorf("after %d skipped fires: skip = %q x%d", SkipBadgeFires, got.LastSkip,
			got.SkippedFires)
	}
	for _, n := range notices.notices() {
		want := &run.Run{
			Kind: run.KindSkippedFire, Source: "schedule", SourceID: "sch_skip",
			TemplateID: "tpl_web", InventoryID: "inv_smart", OrgID: "org_ops", Warning: SkipNoHosts,
			Actor: "system:scheduler", ActorType: "system",
			Labels: map[string]string{"schedule": "nightly patch"},
		}
		if diff := cmp.Diff(want, n, cmpopts.IgnoreFields(run.Run{}, "CreatedAt")); diff != "" {
			t.Errorf("skip notice mismatch (-want +got):\n%s", diff)
		}
	}

	sub.mu.Lock()
	sub.err = nil
	sub.mu.Unlock()
	s.tick(time.Now())
	if got, err = store.Get(ctx, "sch_skip"); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.SkippedFires != 0 || got.LastSkip != "" || got.LastRunID == "" {
		t.Errorf("after a fire that started a run: skip = %q x%d, run %q, want no skip and a run",
			got.LastSkip, got.SkippedFires, got.LastRunID)
	}
}
