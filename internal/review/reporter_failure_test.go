package review

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/trigger"
)

// obxStoreClock is the shared database clock every reporter in a test reads, settable by the test.
type obxStoreClock struct {
	// mu guards now.
	mu sync.Mutex
	// now is the store's current time.
	now time.Time
}

// read returns the store's current time.
func (c *obxStoreClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the store's clock forward.
func (c *obxStoreClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// obxClockedRuns is a run store whose clock is the test's store clock, the way every replica reads
// the database's clock for claims and retries.
type obxClockedRuns struct {
	run.Store
	// clock is the store's clock.
	clock *obxStoreClock
}

// Now answers with the store's clock.
func (r obxClockedRuns) Now(context.Context) (time.Time, error) {
	return r.clock.read(), nil
}

// obxPausedAudits is a chain that holds the first report entry appended through it until the test
// releases it, standing in for a process that stops, in a garbage collection pause, a frozen VM, or
// a SIGSTOP, after it decided what to write to the pull request and before it wrote it.
type obxPausedAudits struct {
	audit.Store
	// entered is closed when the first report entry arrives.
	entered chan struct{}
	// release is closed by the test to let the entry through.
	release chan struct{}
	// once holds only the first report entry.
	once sync.Once
}

// Append holds the first report entry until released, then appends every entry to the chain.
func (p *obxPausedAudits) Append(ctx context.Context, e *audit.Entry) error {
	if strings.HasSuffix(e.Path, "/report") {
		p.once.Do(func() {
			close(p.entered)
			<-p.release
		})
	}
	return p.Store.Append(ctx, e)
}

// TestReviewLapsedClaimWritesNoSecondComment covers the fencing of a claim against a process that
// pauses rather than dies. The documented promise is that two servers never write one pull
// request's comment at the same time, and that a report taken over after its claim lapsed updates
// the same comment rather than adding one. Replica A claims a plan's report, reads the forge, finds
// no comment, and decides to create one, then stops for longer than the claim's six minutes.
// Replica B sees the lapsed claim, takes the report, and posts the comment. When A resumes it
// writes what it decided before it stopped: a second comment, because nothing checks that A still
// holds the claim between deciding and writing. Only the settle afterwards is fenced, and by then
// the forge has the duplicate. Every later report updates the oldest marked comment, so the second
// one keeps the stale phase it was written with for the life of the pull request.
func TestReviewLapsedClaimWritesNoSecondComment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	clock := &obxStoreClock{now: time.Now()}
	paused := &obxPausedAudits{Store: h.audits, entered: make(chan struct{}),
		release: make(chan struct{})}
	a, _ := h.newReporter(t, func(c *Config) {
		c.Runs = obxClockedRuns{Store: h.runs, clock: clock}
		c.Audits = paused
	})
	b, _ := h.newReporter(t, func(c *Config) {
		c.Runs = obxClockedRuns{Store: h.runs, clock: clock}
	})
	sha := strings.Repeat("c", 40)
	h.forge.SetHead(7, sha)
	h.saveRun(t, "run_obx_pause", sha, time.Now(), run.StatusRunning, "")
	if _, err := h.store.Create(ctx, PlanRecord("run_obx_pause", h.trigger, 7,
		clock.read())); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Replica A claims the report and stops after deciding to create the comment.
	a.sweep(ctx)
	select {
	case <-paused.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("replica A never reached the point of writing its report")
	}

	// A stays stopped past its claim. Replica B takes the lapsed claim and reports.
	clock.advance(claimLease + time.Minute)
	b.sweep(ctx)
	b.wg.Wait()
	if n := len(h.forge.Comments(7)); n != 1 {
		t.Fatalf("after replica B reported, the pull request holds %d comments, want 1", n)
	}

	// A resumes.
	close(paused.release)
	a.wg.Wait()
	if c := h.forge.Comments(7); len(c) != 1 {
		t.Errorf("the pull request holds %d review comments after a paused replica resumed, "+
			"want 1: a report whose claim lapsed still wrote to the pull request", len(c))
	}
}

// TestReviewFinalReportMeasuresTheGraceOnOneClock covers clock skew between the process that ran a
// plan and the database every reporter measures by. A final report waits up to two minutes for the
// plan's outcome entry, and the wait is measured from the run's end time, which the executing
// process stamped with its own clock, against the database's clock. On an executor whose clock is
// three minutes behind the database, a plan that ended a moment ago already looks three minutes
// old. In the window between the terminal write and the outcome commit the reporter posts the final
// report at once, saying the outcome did not reach the audit chain and that no receipt can be
// issued, and closes the record. The outcome lands a moment later, and the pull request keeps the
// false statement for good.
func TestReviewFinalReportMeasuresTheGraceOnOneClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	// The database's clock runs three minutes ahead of the clock that stamped the plan's end.
	clock := &obxStoreClock{now: time.Now().Add(3 * time.Minute)}
	rp, _ := h.newReporter(t, func(c *Config) {
		c.Runs = obxClockedRuns{Store: h.runs, clock: clock}
	})
	sha := strings.Repeat("d", 40)
	h.forge.SetHead(7, sha)
	// The plan has just finished in the store, and its outcome has not reached the chain yet.
	h.saveRun(t, "run_obx_skew", sha, time.Now(), run.StatusSucceeded, planLog)
	if _, err := h.store.Create(ctx, PlanRecord("run_obx_skew", h.trigger, 7,
		clock.read())); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rp.sweep(ctx)
	rp.wg.Wait()

	// The outcome lands a moment after the terminal write, as it does on every run.
	h.commitOutcome(t, "run_obx_skew")
	clock.advance(time.Second)
	rp.sweep(ctx)
	rp.wg.Wait()

	comments := h.forge.Comments(7)
	if len(comments) != 1 {
		t.Fatalf("the pull request holds %d comments, want 1", len(comments))
	}
	if strings.Contains(comments[0].Body, "did not reach the audit chain") {
		t.Errorf("the final report says the plan's outcome is missing from the chain, while the "+
			"outcome was committed a moment after the plan ended; record = %+v",
			h.record(t, "run_obx_skew"))
	}
}

// TestReviewReportStaysWithItsRepository covers a review trigger edited while a report it owes is
// still pending. A report record keeps the trigger and the pull request number and nothing else,
// and the repository, the forge, and the token are read from the trigger when the report goes out.
// A plan of pull request 7 in one repository is recorded, its report waits, as it does while the
// plan runs or while a forge outage is retried for up to a day, and the trigger is then pointed at
// another repository, which the API allows. The report is posted to pull request 7 of the other
// repository: a different pull request, possibly in a repository with a different audience,
// receives the plan's verdict and its masked output, and a commit status for a commit it does not
// hold.
func TestReviewReportStaysWithItsRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	rp, _ := h.newReporter(t, nil)
	// The webhook arrives while the trigger reviews acme/private, and its plan is recorded.
	planned := *h.trigger
	review := *h.trigger.Review
	review.Repository = "acme/private"
	planned.Review = &review
	if err := h.triggers.Save(ctx, &planned); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sha := strings.Repeat("e", 40)
	h.forge.SetHead(7, sha)
	h.planRun(t, "run_obx_repo", sha, time.Now(), run.StatusSucceeded, planLog)
	if _, err := h.store.Create(ctx, PlanRecord("run_obx_repo", &planned, 7,
		time.Now())); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Before the report goes out, the trigger is pointed at acme/infra.
	if err := h.triggers.Save(ctx, h.trigger); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	rp.sweep(ctx)
	rp.wg.Wait()

	if c := h.forge.Comments(7); len(c) != 0 {
		t.Errorf("a plan of acme/private pull request 7 was posted to acme/infra pull request 7: "+
			"%d comments, first:\n%s", len(c), c[0].Body)
	}
	if s := h.forge.Statuses(sha); len(s) != 0 {
		t.Errorf("acme/infra was sent %d commit statuses for a commit of acme/private", len(s))
	}
}

// TestReviewStatusEndsUnderTheContextItStarted covers a template renamed while its plan is in
// flight. The commit status a plan sets is named switchtender/<template name>, and the name is read
// again for every report. A plan reported running, or waiting hours for an approver, sets a pending
// status under the old name. The template is renamed, and the plan's result is set under the new
// name, so the status under the old name stays pending on that commit forever. A branch protection
// rule that requires the status the pull request was first given never sees it finish.
func TestReviewStatusEndsUnderTheContextItStarted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	rp, _ := h.newReporter(t, nil)
	sha := strings.Repeat("f", 40)
	h.forge.SetHead(7, sha)
	h.saveRun(t, "run_obx_rename", sha, time.Now(), run.StatusRunning, "")
	if _, err := h.store.Create(ctx, PlanRecord("run_obx_rename", h.trigger, 7,
		time.Now())); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rp.sweep(ctx)
	rp.wg.Wait()

	tpl, err := h.templates.Get(ctx, "tpl_net")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	tpl.Name = "network-prod"
	if err := h.templates.Save(ctx, tpl); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	h.finish(t, "run_obx_rename", run.StatusSucceeded)
	rp.sweep(ctx)
	rp.wg.Wait()

	latest := map[string]string{}
	for _, s := range h.forge.Statuses(sha) {
		latest[s.Context] = s.State
	}
	for name, state := range latest {
		if state == StatePending {
			t.Errorf("the plan finished and the status %s it set on the commit is still %s: "+
				"latest states by context = %v", name, state, latest)
		}
	}
}
