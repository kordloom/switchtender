package schedule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// errStore wraps a Store and fails whichever calls a test asks it to, standing in for a database
// that is unreachable or a row another node already claimed.
type errStore struct {
	// Store is the real store underneath.
	Store
	// listErr is returned by List when set.
	listErr error
	// claimErr is returned by ClaimDue when set.
	claimErr error
	// claimLost makes ClaimDue report that another caller won.
	claimLost bool
	// recordErr is returned by RecordFire when set.
	recordErr error
	// claims counts ClaimDue calls.
	claims atomic.Int64
}

// List returns the configured error, or the wrapped store's answer.
func (e *errStore) List(ctx context.Context) ([]*Schedule, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return e.Store.List(ctx)
}

// ClaimDue reports the configured outcome, or the wrapped store's.
func (e *errStore) ClaimDue(ctx context.Context, id string, oldNext, newNext time.Time) (bool, error) {
	e.claims.Add(1)
	if e.claimErr != nil {
		return false, e.claimErr
	}
	if e.claimLost {
		return false, nil
	}
	return e.Store.ClaimDue(ctx, id, oldNext, newNext)
}

// RecordFire returns the configured error, or records through the wrapped store.
func (e *errStore) RecordFire(ctx context.Context, id string, at time.Time, runID string) error {
	if e.recordErr != nil {
		return e.recordErr
	}
	return e.Store.RecordFire(ctx, id, at, runID)
}

// failingSubmitter refuses every submission, standing in for a dispatcher that cannot accept work.
type failingSubmitter struct {
	// calls counts refused submissions.
	calls atomic.Int64
}

// Submit refuses.
func (f *failingSubmitter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run, error) {
	f.calls.Add(1)
	return nil, errors.New("dispatcher unavailable")
}

// SubmitSplit refuses.
func (f *failingSubmitter) SubmitSplit(context.Context, string, string, int, ...run.SubmitOption) (*run.Run, error) {
	f.calls.Add(1)
	return nil, errors.New("dispatcher unavailable")
}

// SubmitPipeline refuses.
func (f *failingSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep, ...run.SubmitOption) (*run.Run, error) {
	f.calls.Add(1)
	return nil, errors.New("dispatcher unavailable")
}

// countingKindSubmitter records every submission and which path it took, safely under concurrency.
type countingKindSubmitter struct {
	// calls counts submissions.
	calls atomic.Int64
	// mu guards kind.
	mu sync.Mutex
	// kind records the most recent submit path.
	kind string
}

// Submit records a single submission.
func (c *countingKindSubmitter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run, error) {
	return c.record("single"), nil
}

// SubmitSplit records a split submission.
func (c *countingKindSubmitter) SubmitSplit(context.Context, string, string, int, ...run.SubmitOption) (*run.Run, error) {
	return c.record("split"), nil
}

// SubmitPipeline records a pipeline submission.
func (c *countingKindSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep, ...run.SubmitOption) (*run.Run, error) {
	return c.record("pipeline"), nil
}

// record notes one submission of the given kind and returns the run it stands for.
func (c *countingKindSubmitter) record(kind string) *run.Run {
	c.calls.Add(1)
	c.mu.Lock()
	c.kind = kind
	c.mu.Unlock()
	return &run.Run{ID: "run_counted"}
}

// lastKind returns the most recent submit path.
func (c *countingKindSubmitter) lastKind() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kind
}

// dueSchedule returns an enabled schedule that came due a minute ago.
func dueSchedule(id, cron string) *Schedule {
	past := time.Now().Add(-time.Minute)
	return &Schedule{
		ID: id, Name: id, Cron: cron, Playbook: "site.yml", Enabled: true,
		CreatedAt: time.Now().Add(-time.Hour), NextRunAt: &past,
	}
}

// TestNewSchedulerRefusesToRunWithoutItsDependencies pins what the constructor installs and what it
// refuses, since a scheduler missing a piece would fail later and quietly.
//
// A nil store or submitter is a programming error at startup, and panicking there is what makes it
// visible before the process starts firing unattended work. A nil logger is not an error, but it has
// to be replaced rather than dereferenced on the first tick.
func TestNewSchedulerRefusesToRunWithoutItsDependencies(t *testing.T) {
	t.Parallel()
	t.Run("test 0", func(t *testing.T) { // A nil store panics.
		t.Parallel()
		defer func() {
			if recover() == nil {
				t.Error("NewScheduler() accepted a nil store, so the failure surfaces on the first tick")
			}
		}()
		NewScheduler(nil, &countingKindSubmitter{}, zap.NewNop())
	})
	t.Run("test 1", func(t *testing.T) { // A nil submitter panics.
		t.Parallel()
		defer func() {
			if recover() == nil {
				t.Error("NewScheduler() accepted a nil submitter")
			}
		}()
		NewScheduler(NewMemStore(), nil, zap.NewNop())
	})
	t.Run("test 2", func(t *testing.T) { // A nil logger is replaced, not dereferenced.
		t.Parallel()
		store := NewMemStore()
		if err := store.Save(context.Background(), dueSchedule("s1", "* * * * *")); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		sub := &countingKindSubmitter{}
		s := NewScheduler(store, sub, nil)
		if s.log == nil {
			t.Fatal("the scheduler kept a nil logger")
		}
		s.tick(time.Now())
		if sub.calls.Load() != 1 {
			t.Errorf("fired %d times with a nil logger, want 1", sub.calls.Load())
		}
	})
	t.Run("test 3", func(t *testing.T) { // The default check interval is installed.
		t.Parallel()
		s := NewScheduler(NewMemStore(), &countingKindSubmitter{}, zap.NewNop())
		if s.interval != DefaultInterval {
			t.Errorf("interval = %v, want the default %v", s.interval, DefaultInterval)
		}
		if s.templates != nil || s.audits != nil || s.runActive != nil {
			t.Error("an option nobody asked for was installed")
		}
	})
}

// TestSchedulerOptionsInstallWhatTheyName pins each option against the field it sets, because an
// option wired to the wrong field passes every behavior test that does not use it.
//
// The interval guard matters most: a zero or negative interval handed to a ticker panics, and it
// would take the scheduler goroutine and the process with it.
func TestSchedulerOptionsInstallWhatTheyName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Interval is the value handed to WithInterval.
		Interval time.Duration
		// WantInterval is the interval the scheduler must end up with.
		WantInterval time.Duration
	}{
		{Interval: time.Second, WantInterval: time.Second},         // Test 0: A sensible value.
		{Interval: time.Nanosecond, WantInterval: time.Nanosecond}, // Test 1: A tiny positive value.
		{Interval: 0, WantInterval: DefaultInterval},               // Test 2: Zero is ignored, since
		// a ticker built from it would panic and take the loop down.
		{Interval: -time.Second, WantInterval: DefaultInterval}, // Test 3: Negative is ignored too.
		{Interval: time.Hour, WantInterval: time.Hour},          // Test 4: A long interval.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := NewScheduler(NewMemStore(), &countingKindSubmitter{}, zap.NewNop(),
				WithInterval(test.Interval))
			if s.interval != test.WantInterval {
				t.Errorf("interval = %v, want %v", s.interval, test.WantInterval)
			}
		})
	}
	// The remaining options install the collaborators they name.
	templates := template.NewMemStore()
	s := NewScheduler(NewMemStore(), &countingKindSubmitter{}, zap.NewNop(),
		WithTemplates(templates),
		WithRunActive(func(context.Context, string) (bool, error) { return true, nil }))
	if s.templates == nil {
		t.Error("WithTemplates installed no template store, so a scheduled template would refuse")
	}
	if s.runActive == nil {
		t.Error("WithRunActive installed no overlap check, so a slow run would stack on itself")
	}
	// The last option wins, so a caller reconfiguring one does not end up with both.
	again := NewScheduler(NewMemStore(), &countingKindSubmitter{}, zap.NewNop(),
		WithInterval(time.Minute), WithInterval(2*time.Minute))
	if again.interval != 2*time.Minute {
		t.Errorf("interval = %v, want the last option to win", again.interval)
	}
}

// TestSchedulerStartsAndStopsCleanly pins the loop's lifecycle: it fires on its own cadence once
// started, and Close stops it and waits for it to be finished.
//
// Close returning before the loop is done would let a shutdown race a fire, and a loop that keeps
// running after Close would fire work during shutdown, which is the one moment nobody is watching
// for it.
func TestSchedulerStartsAndStopsCleanly(t *testing.T) {
	t.Parallel()
	store := NewMemStore()
	ctx := context.Background()
	if err := store.Save(ctx, dueSchedule("s1", "* * * * *")); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &countingKindSubmitter{}
	s := NewScheduler(store, sub, zap.NewNop(), WithInterval(time.Millisecond))
	s.Start()

	deadline := time.Now().Add(2 * time.Second)
	for sub.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sub.calls.Load() == 0 {
		t.Fatal("the started scheduler never fired a due schedule")
	}

	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return, so a shutdown would hang")
	}
	// Nothing fires after Close, however long the loop is left alone.
	settled := sub.calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := sub.calls.Load(); got != settled {
		t.Errorf("the scheduler fired %d more times after Close", got-settled)
	}
	if s.ctx.Err() == nil {
		t.Error("Close() left the scheduler context live")
	}
}

// TestTickSurvivesEveryStoreFailure pins that a store that cannot answer stops the tick rather than
// firing on a guess.
//
// Everything the tick does after listing depends on the list being right. A read failure has to end
// the tick quietly, a lost claim has to skip that schedule so two nodes never both fire it, and a
// claim error has to skip it too rather than fire without having won the row.
func TestTickSurvivesEveryStoreFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which failure is injected.
		Name string
		// Configure sets the failure on the store.
		Configure func(e *errStore)
		// WantFires is how many runs must be submitted.
		WantFires int64
	}{{ // Test 0: The list fails, so the tick does nothing at all.
		Name: "list fails", Configure: func(e *errStore) { e.listErr = errors.New("db down") },
		WantFires: 0,
	}, { // Test 1: Another node won the row, so this one skips rather than double-launching.
		Name: "claim lost", Configure: func(e *errStore) { e.claimLost = true }, WantFires: 0,
	}, { // Test 2: The claim itself failed, so the row was never won and nothing may fire.
		Name: "claim errors", Configure: func(e *errStore) { e.claimErr = errors.New("db down") },
		WantFires: 0,
	}, { // Test 3: Recording the fire fails after the run was created, which must not stop the run
		// that already exists from having been submitted.
		Name: "record fails", Configure: func(e *errStore) { e.recordErr = errors.New("db down") },
		WantFires: 1,
	}, { // Test 4: Nothing fails, as the control.
		Name: "healthy", Configure: func(*errStore) {}, WantFires: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			inner := NewMemStore()
			if err := inner.Save(context.Background(), dueSchedule("s1", "* * * * *")); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			store := &errStore{Store: inner}
			test.Configure(store)
			sub := &countingKindSubmitter{}
			NewScheduler(store, sub, zap.NewNop()).tick(time.Now())
			if got := sub.calls.Load(); got != test.WantFires {
				t.Errorf("%s: fired %d times, want %d", test.Name, got, test.WantFires)
			}
		})
	}
}

// TestTickRefusesToFireAnUnfireableSchedule pins what happens when a stored schedule cannot produce
// a next fire time.
//
// The row is read as due on every tick, so the direction this fails in decides between a schedule
// that sits there doing nothing and one that fires continuously. It must not fire, and it must not
// be claimed either, because claiming it would advance a next-run time that could not be computed.
func TestTickRefusesToFireAnUnfireableSchedule(t *testing.T) {
	t.Parallel()
	inner := NewMemStore()
	ctx := context.Background()
	// A row like this cannot be created through validation, but a restore or a hand-edited database
	// can produce one, and the tick loop is what meets it.
	bad := dueSchedule("s_bad", "0 0 30 2 *")
	if err := inner.Save(ctx, bad); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	worse := dueSchedule("s_worse", "not a cron")
	if err := inner.Save(ctx, worse); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	store := &errStore{Store: inner}
	sub := &countingKindSubmitter{}
	s := NewScheduler(store, sub, zap.NewNop())

	for range 3 {
		s.tick(time.Now())
	}
	if got := sub.calls.Load(); got != 0 {
		t.Errorf("a schedule that can never come due fired %d times", got)
	}
	if got := store.claims.Load(); got != 0 {
		t.Errorf("the row was claimed %d times despite having no next fire time", got)
	}
	for _, id := range []string{"s_bad", "s_worse"} {
		got, err := inner.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if got.LastRunID != "" || got.LastRunAt != nil {
			t.Errorf("%s recorded a fire that never happened: %+v", id, got)
		}
	}
}

// TestTickSkipsWhatIsNotDue pins the boundary of due-ness, which decides whether unattended work
// runs early.
//
// A schedule is due when its next run time is not after now, so the instant itself counts. A
// disabled schedule and one with no next run time never count, whatever the clock says.
func TestTickSkipsWhatIsNotDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	exactly := now
	aSecondLater := now.Add(time.Second)
	aSecondEarlier := now.Add(-time.Second)
	tests := []struct {
		// Schedule is the stored row.
		Schedule *Schedule
		// WantFires is how many runs the tick must submit.
		WantFires int64
	}{{ // Test 0: Due a second ago.
		Schedule: &Schedule{
			ID: "s", Cron: "* * * * *", Playbook: "p", Enabled: true, NextRunAt: &aSecondEarlier,
		}, WantFires: 1,
	}, { // Test 1: Due at exactly this instant, which counts as due.
		Schedule: &Schedule{
			ID: "s", Cron: "* * * * *", Playbook: "p", Enabled: true, NextRunAt: &exactly,
		}, WantFires: 1,
	}, { // Test 2: Due a second from now, which does not.
		Schedule: &Schedule{
			ID: "s", Cron: "* * * * *", Playbook: "p", Enabled: true, NextRunAt: &aSecondLater,
		}, WantFires: 0,
	}, { // Test 3: Due, but disabled.
		Schedule: &Schedule{
			ID: "s", Cron: "* * * * *", Playbook: "p", Enabled: false, NextRunAt: &aSecondEarlier,
		}, WantFires: 0,
	}, { // Test 4: Enabled with no next run time at all, which is a schedule nothing has scheduled.
		Schedule: &Schedule{ID: "s", Cron: "* * * * *", Playbook: "p", Enabled: true}, WantFires: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := NewMemStore()
			if err := store.Save(context.Background(), test.Schedule); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &countingKindSubmitter{}
			NewScheduler(store, sub, zap.NewNop()).tick(now)
			if got := sub.calls.Load(); got != test.WantFires {
				t.Errorf("fired %d times, want %d", got, test.WantFires)
			}
		})
	}
}

// TestAFailedFireDoesNotRewriteTheLastRunRecord pins what a fire that could not be submitted leaves
// behind.
//
// The schedule still advanced, since the slot passed, but the run id has to stay as it was: a failed
// fire created nothing, and overwriting the last run id with an empty one would break the link from
// the schedule to the last run it actually produced, which is the link an operator follows to find
// out what happened.
func TestAFailedFireDoesNotRewriteTheLastRunRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	sc := dueSchedule("s1", "* * * * *")
	sc.LastRunID = "run_previous"
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &failingSubmitter{}
	NewScheduler(store, sub, zap.NewNop()).tick(time.Now())

	if sub.calls.Load() != 1 {
		t.Fatalf("the submitter was called %d times, want 1", sub.calls.Load())
	}
	got, err := store.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastRunID != "run_previous" {
		t.Errorf("LastRunID = %q, want the previous run: a failed fire created nothing to point at",
			got.LastRunID)
	}
	if got.NextRunAt == nil || !got.NextRunAt.After(time.Now()) {
		t.Errorf("NextRunAt = %v, want the schedule advanced past the slot that failed", got.NextRunAt)
	}
}

// TestOverlapCheckFailsTowardFiring pins the direction the overlap check errs in when it cannot read
// the previous run.
//
// Refusing to fire because the run store could not be read would turn a transient database problem
// into silently skipped automation, which is the failure nobody notices. Firing may at worst produce
// the overlap the check exists to avoid, which is visible.
func TestOverlapCheckFailsTowardFiring(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the overlap check reports.
		Name string
		// LastRunID is the run recorded on the schedule.
		LastRunID string
		// Active is what the check returns.
		Active bool
		// Err is what the check reports instead.
		Err error
		// WantFires is how many runs the tick must submit.
		WantFires int64
	}{{ // Test 0: The previous run is still going, so this fire is skipped.
		Name: "still going", LastRunID: "run_prev", Active: true, WantFires: 0,
	}, { // Test 1: The previous run finished, so the schedule fires.
		Name: "finished", LastRunID: "run_prev", Active: false, WantFires: 1,
	}, { // Test 2: The check could not read the run store, so the schedule fires rather than going
		// silently dark.
		Name: "unreadable", LastRunID: "run_prev", Err: errors.New("db down"), WantFires: 1,
	}, { // Test 3: There is no previous run to wait for, so the check is never consulted.
		Name: "no previous run", LastRunID: "", Active: true, WantFires: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			sc := dueSchedule("s1", "* * * * *")
			sc.LastRunID = test.LastRunID
			if err := store.Save(ctx, sc); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &countingKindSubmitter{}
			s := NewScheduler(store, sub, zap.NewNop(),
				WithRunActive(func(context.Context, string) (bool, error) {
					return test.Active, test.Err
				}))
			s.tick(time.Now())
			if got := sub.calls.Load(); got != test.WantFires {
				t.Errorf("%s: fired %d times, want %d", test.Name, got, test.WantFires)
			}
			// Either way the schedule stays on its cadence rather than firing the moment the slow run
			// ends.
			got, err := store.Get(ctx, "s1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.NextRunAt == nil || !got.NextRunAt.After(time.Now()) {
				t.Errorf("%s: NextRunAt = %v, want it advanced past the slot", test.Name, got.NextRunAt)
			}
		})
	}

	// A skipped fire whose advance cannot be written is still a skipped fire. The schedule stays
	// where it was and will be seen as due again, which is the safe direction: the alternative is
	// firing on top of a run that is still going because the row could not be updated.
	t.Run("test 4", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		inner := NewMemStore()
		sc := dueSchedule("s1", "* * * * *")
		sc.LastRunID = "run_prev"
		if err := inner.Save(ctx, sc); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		store := &errStore{Store: inner, claimErr: errors.New("db down")}
		sub := &countingKindSubmitter{}
		s := NewScheduler(store, sub, zap.NewNop(),
			WithRunActive(func(context.Context, string) (bool, error) { return true, nil }))
		s.tick(time.Now())
		if got := sub.calls.Load(); got != 0 {
			t.Errorf("fired %d times while the previous run was going and the advance failed", got)
		}
		if got := store.claims.Load(); got != 1 {
			t.Errorf("claimed %d times, want the one attempt to advance past the skipped slot", got)
		}
	})
}

// TestATemplateScheduleRefusesRatherThanFiringSomethingElse pins the failures on the template path.
//
// A schedule naming a template carries no playbook of its own, so if the template cannot be resolved
// there is nothing to run. Falling through to the plain submit path would fire a run with an empty
// playbook, which is a scheduled job that appears to have run and did nothing.
func TestATemplateScheduleRefusesRatherThanFiringSomethingElse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("test 0", func(t *testing.T) { // No template store configured at all.
		t.Parallel()
		sub := &countingKindSubmitter{}
		s := NewScheduler(NewMemStore(), sub, zap.NewNop())
		_, err := s.fire(ctx, &Schedule{ID: "sch_1", TemplateID: "tpl_1"})
		if err == nil {
			t.Fatal("a schedule naming a template fired with no template store configured")
		}
		if sub.calls.Load() != 0 {
			t.Error("something was submitted for a template that could not be resolved")
		}
	})

	t.Run("test 1", func(t *testing.T) { // The template is gone.
		t.Parallel()
		sub := &countingKindSubmitter{}
		s := NewScheduler(NewMemStore(), sub, zap.NewNop(), WithTemplates(template.NewMemStore()))
		_, err := s.fire(ctx, &Schedule{ID: "sch_1", TemplateID: "tpl_missing"})
		if err == nil {
			t.Fatal("a schedule naming a deleted template fired anyway")
		}
		if !errors.Is(err, template.ErrNotFound) {
			t.Errorf("fire() error = %v, want it to wrap the template store's not-found so the "+
				"reason is legible", err)
		}
		if sub.calls.Load() != 0 {
			t.Error("something was submitted for a template that no longer exists")
		}
	})

	t.Run("test 2", func(t *testing.T) { // A sharded template fires a split.
		t.Parallel()
		templates := template.NewMemStore()
		if err := templates.Save(ctx, &template.Template{
			ID: "tpl_split", Name: "spread", Playbook: "site.yml", Inventory: "prod", Shards: 4,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		sub := &countingKindSubmitter{}
		s := NewScheduler(NewMemStore(), sub, zap.NewNop(), WithTemplates(templates))
		if _, err := s.fire(ctx, &Schedule{ID: "sch_1", TemplateID: "tpl_split"}); err != nil {
			t.Fatalf("fire() error = %v", err)
		}
		if got := sub.lastKind(); got != "split" {
			t.Errorf("kind = %q, want split: a sharded template did not fire its shards", got)
		}
	})

	t.Run("test 3", func(t *testing.T) { // A single-shard template fires a single run.
		t.Parallel()
		templates := template.NewMemStore()
		if err := templates.Save(ctx, &template.Template{
			ID: "tpl_one", Name: "one", Playbook: "site.yml", Inventory: "prod", Shards: 1,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		sub := &countingKindSubmitter{}
		s := NewScheduler(NewMemStore(), sub, zap.NewNop(), WithTemplates(templates))
		if _, err := s.fire(ctx, &Schedule{ID: "sch_1", TemplateID: "tpl_one"}); err != nil {
			t.Fatalf("fire() error = %v", err)
		}
		if got := sub.lastKind(); got != "single" {
			t.Errorf("kind = %q, want single: one shard is not a split", got)
		}
	})
}

// TestTwoSchedulersNeverDoubleLaunchTheSameSlot pins the guarantee a highly available pair rests on.
//
// Both nodes list the same rows and both see the same schedule as due. Only the node that wins the
// row may fire, or the same playbook runs twice against the same hosts at the same moment, which for
// a non-idempotent play is a real change made twice with nothing saying so.
func TestTwoSchedulersNeverDoubleLaunchTheSameSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	if err := store.Save(ctx, dueSchedule("s1", "0 2 * * *")); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &countingKindSubmitter{}
	now := time.Now()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			NewScheduler(store, sub, zap.NewNop()).tick(now)
		}()
	}
	wg.Wait()
	if got := sub.calls.Load(); got != 1 {
		t.Errorf("the same slot fired %d times across concurrent schedulers, want exactly 1", got)
	}
}

// TestRecordFireEntryIsSkippedWithoutAChain pins that a scheduler with no audit store still fires,
// and hands the submit path a context it did not touch.
//
// Recording is required where a chain is configured and impossible where one is not, so the absence
// of a chain must not be treated as a failed append and must not attach a receipt to a run that has
// no entry behind it.
func TestRecordFireEntryIsSkippedWithoutAChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewScheduler(NewMemStore(), &countingKindSubmitter{}, zap.NewNop())
	got, err := s.recordFireEntry(ctx, &Schedule{ID: "sch_1", Name: "nightly"})
	if err != nil {
		t.Fatalf("recordFireEntry() error = %v", err)
	}
	if got != ctx {
		t.Error("the context was replaced with no chain configured, so a run would carry a receipt " +
			"for an entry that does not exist")
	}
	if receipt := run.AuditReceiptFrom(got); receipt != "" {
		t.Errorf("the context carries receipt %q with no chain configured", receipt)
	}
}
