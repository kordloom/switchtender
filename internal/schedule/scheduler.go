package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
)

// DefaultInterval is how often the scheduler checks for due schedules when none is configured.
const DefaultInterval = 15 * time.Second

// maxFailure bounds the bytes of a failed fire's reason a schedule keeps.
const maxFailure = 1000

// Submitter fires a schedule's target. The dispatcher satisfies it.
type Submitter interface {
	// Submit fires a single run.
	Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error)
	// SubmitSplit fires a split run.
	SubmitSplit(ctx context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error)
	// SubmitPipeline fires a pipeline run.
	SubmitPipeline(ctx context.Context, name, inventory string, steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error)
}

// Scheduler fires due schedules on a fixed cadence.
type Scheduler struct {
	// store reads and updates schedules.
	store Store
	// submitter fires a schedule's target.
	submitter Submitter
	// templates resolves schedules that fire stored templates, nil when unused.
	templates template.Store
	// runActive reports whether a schedule's previous run is still going, nil when overlap is allowed.
	runActive RunActive
	// audits records each fire as a chain entry before the run exists, nil when no trail is kept.
	// With it, a scheduled run carries a creation receipt like any other and can be receipted.
	audits audit.Store
	// skips tells a schedule's attached notification targets when a fire is skipped, nil when
	// nothing is told.
	skips SkipNotifier
	// log records scheduler activity.
	log *zap.Logger
	// interval is how often due schedules are checked.
	interval time.Duration
	// ctx is canceled by Close to stop the loop.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelFunc
	// done closes when the loop exits.
	done chan struct{}
	// startOnce launches the loop at most once, so a second Start cannot leave two loops firing the
	// same schedules or close done twice.
	startOnce sync.Once
	// started reports whether the loop was ever launched, which is what Close waits on.
	started atomic.Bool
}

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithTemplates lets schedules fire stored job templates by id.
func WithTemplates(store template.Store) SchedulerOption {
	return func(s *Scheduler) { s.templates = store }
}

// WithAudits records each fire on the tamper-evident chain before the run is created, so a
// scheduled run has the same creation evidence as one a person requested.
func WithAudits(store audit.Store) SchedulerOption {
	return func(s *Scheduler) { s.audits = store }
}

// RunActive returns the id of a run of the schedule's that is still going, given the schedule and
// the run it last created, or the empty string when none is. A schedule does not start a second
// copy of work the first has not finished, and the id says in the log which run it is waiting on.
type RunActive func(ctx context.Context, scheduleID, lastRunID string) (string, error)

// WithRunActive makes a schedule wait for its own previous run rather than stacking on top of it.
//
// A schedule fired whenever its next run time came due, with no regard for what it started last
// time. A five-minute schedule whose playbook takes eight minutes therefore accumulated concurrent
// copies of itself against the same hosts, and Ansible tasks that are not idempotent interleaved:
// two runs installing a package, restarting a service, or holding the same lock, with neither aware
// of the other. Nothing capped how many piled up.
//
// Skipping is the safe direction. A run that is simply slow gets left alone until it finishes and
// the schedule resumes on its normal cadence, where firing anyway compounds whatever made it slow.
// Without this option the old behavior stands, so a caller that wants overlap keeps it.
func WithRunActive(active RunActive) SchedulerOption {
	return func(s *Scheduler) { s.runActive = active }
}

// RunReader is the part of a run store the overlap check reads.
type RunReader interface {
	// Get returns the run with the given id, or run.ErrNotFound.
	Get(ctx context.Context, id string) (*run.Run, error)
	// ListPage returns the top-level runs matching filter, newest first.
	ListPage(ctx context.Context, filter run.ListFilter, limit, offset int) ([]*run.Run, error)
}

// ActiveIn returns the overlap check a server wires: a schedule's work is still going while the run
// it last created, or any run fired under its name, has not finished.
//
// Reading the last run alone missed the apply a plan gate proposes. A scheduled terraform apply
// under a plan-content rule is planned first, and the plan run finishes the moment it proposes the
// apply, so the next tick found the schedule's last run done and fired another plan while the first
// apply was still held for a person or running. Each tick added a held apply, and approving the
// queue ran them one after another. The proposed apply carries the plan's source, so asking for any
// unfinished run the schedule fired finds it, and finds one the retention sweep left behind a
// pruned last run.
func ActiveIn(runs RunReader) RunActive {
	return func(ctx context.Context, scheduleID, lastRunID string) (string, error) {
		// The run the schedule last created is read by id first. It is the common case, and a run
		// stored before runs recorded their source is found no other way.
		if lastRunID != "" {
			r, err := runs.Get(ctx, lastRunID)
			switch {
			case err == nil && !r.Status.Terminal():
				return r.ID, nil
			case err != nil && !errors.Is(err, run.ErrNotFound):
				return "", err
			}
		}
		for _, status := range []run.Status{
			run.StatusPending, run.StatusPendingApproval, run.StatusRunning,
		} {
			found, err := runs.ListPage(ctx, run.ListFilter{
				Source: "schedule", SourceID: scheduleID, Status: string(status),
			}, 1, 0)
			if err != nil {
				return "", err
			}
			if len(found) > 0 {
				return found[0].ID, nil
			}
		}
		return "", nil
	}
}

// WithInterval sets how often due schedules are checked. Values below one are ignored.
func WithInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.interval = d
		}
	}
}

// NewScheduler returns a Scheduler. It panics if store or submitter is nil; a nil logger is a no-op.
func NewScheduler(store Store, submitter Submitter, log *zap.Logger, opts ...SchedulerOption) *Scheduler {
	if store == nil {
		panic("schedule: Store required")
	}
	if submitter == nil {
		panic("schedule: Submitter required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		store: store, submitter: submitter, log: log, interval: DefaultInterval,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start begins the scheduler loop in a background goroutine. A second call does nothing: two loops
// over one store would race each other's ClaimDue on every due row, and whichever exited second
// would close done a second time, which panics and takes the process with it.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		s.started.Store(true)
		go func() {
			defer close(s.done)
			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case t := <-ticker.C:
					s.tick(t)
				}
			}
		}()
	})
}

// Close stops the scheduler loop and waits for it to exit. It returns at once on a scheduler that
// was never started: done is closed by the loop and by nothing else, so waiting on it when no loop
// was ever launched blocked the caller forever. That turned a startup that gave up before Start
// into a process that hangs in shutdown rather than one that exits with the error, and the shutdown
// goroutine and everything it held stayed alive for the life of the process.
func (s *Scheduler) Close() {
	s.cancel()
	if !s.started.Load() {
		return
	}
	<-s.done
}

// settleTimeout bounds each write that settles a claim the scheduler made: the claim itself, the
// record of the fire, and handing back an occurrence a stop cut short. These run on a context Close
// does not cancel, so they need a bound of their own.
const settleTimeout = 15 * time.Second

// settleContext returns the context a claim and its records are written on. It outlives Close,
// because a claim that is won and then never recorded is an occurrence that silently did not run.
func (s *Scheduler) settleContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), settleTimeout)
}

// tick fires every schedule due at now and advances its next run time.
func (s *Scheduler) tick(now time.Time) {
	schedules, err := s.store.List(s.ctx)
	if err != nil {
		if s.ctx.Err() == nil {
			s.log.Error("schedule: list: " + err.Error())
		}
		return
	}
	for _, sc := range schedules {
		// A scheduler told to stop claims nothing more. A fire it has already claimed settles
		// whichever way it ends, so a stop never leaves a claim behind with no account of it.
		if s.ctx.Err() != nil {
			return
		}
		if !sc.Enabled || sc.NextRunAt == nil || sc.NextRunAt.After(now) {
			continue
		}
		s.fireDue(sc, now)
	}
}

// fireDue claims and fires one schedule whose occurrence came due, on a tick at now.
func (s *Scheduler) fireDue(sc *Schedule, now time.Time) {
	due := *sc.NextRunAt
	// A recurrence bounded by COUNT or UNTIL has a last occurrence, and this may be it. It is still
	// fired, since it came due, and the row is claimed by clearing its next fire time rather than
	// advancing it, so the schedule stops instead of logging an error every tick.
	//
	// The next fire is worked out from the occurrence that came due as well as from now. Worked out
	// from now alone, a tick that landed late on the night the clocks go back took the second
	// reading of the wall clock time it had just fired as the next fire, and fired it again.
	next, err := sc.NextFireAfter(due, now)
	final := errors.Is(err, ErrExhausted)
	if err != nil && !final {
		s.log.Error("schedule: next fire: "+err.Error(), zap.String("schedule_id", sc.ID))
		return
	}
	// A schedule does not start a second copy of work its own previous run has not finished. This is
	// checked before the row is claimed so a skipped tick still advances the next run time, which is
	// what keeps the schedule on its cadence instead of firing the moment the slow run ends.
	if waiting := s.overlaps(sc); waiting != "" {
		if _, err := s.claim(sc, next, final); err != nil {
			s.log.Error("schedule: advance past overlap: "+err.Error(),
				zap.String("schedule_id", sc.ID))
		}
		s.log.Warn("schedule: a run this schedule fired is still going, skipping this fire",
			zap.String("schedule_id", sc.ID), zap.String("run_id", waiting))
		return
	}
	// The overlap check reads the run store, and a stop that landed during it ends the tick here,
	// before anything is claimed.
	if s.ctx.Err() != nil {
		return
	}
	// Win the row before firing so concurrent scheduler instances never double-launch.
	won, err := s.claim(sc, next, final)
	if err != nil {
		s.log.Error("schedule: claim due: "+err.Error(), zap.String("schedule_id", sc.ID))
		return
	}
	if !won {
		return
	}

	runID, err := s.fire(s.ctx, sc)
	switch {
	case skipped(err):
		// A fire whose inventory matched no hosts is skipped, not failed. skip owns its record.
		s.skip(sc, now)
		return
	case err != nil && runID == "" && s.ctx.Err() != nil:
		s.handBack(sc, due, next, final, now, err)
		return
	}
	failure := ""
	if err != nil {
		s.log.Error("schedule: fire: "+err.Error(), zap.String("schedule_id", sc.ID))
		failure = util.Clip(err.Error(), maxFailure)
	}
	// Only what the fire owns is written back. sc came from the List above, so it is a snapshot
	// taken before the run and writing it whole reverted anything an operator changed meanwhile: a
	// disable came back enabled, an edit was rolled back, and a delete was re-inserted as a live
	// schedule that kept firing. NextRunAt is deliberately not written either, because ClaimDue
	// already advanced it and rewriting it here is what reverted an edited cron.
	ctx, cancel := s.settleContext()
	defer cancel()
	if err := s.store.RecordFire(ctx, sc.ID, now, runID, failure); err != nil {
		s.log.Error("schedule: record fire: "+err.Error(), zap.String("schedule_id", sc.ID))
	}
}

// handBack returns an occurrence whose fire a stop cut short before it started a run, so the next
// scheduler to tick, this one after a restart or the other server of a pair, fires it.
//
// The claim moved the next fire time before the fire, and the fire ran on the context Close
// cancels. A stop between the two therefore canceled the submit and then the write recording why
// nothing ran, and the occurrence was gone: a bounded rule ended finished with no run, no error,
// and no last fire, while the chain held a fire entry for it. The occurrence is put back with a
// compare-and-set on what the claim wrote, so an edit or a delete made meanwhile wins, and the fire
// that takes it up again carries the same idempotency key, so a submit that did land before the
// stop is found rather than repeated. The schedule says what happened either way.
func (s *Scheduler) handBack(sc *Schedule, due, next time.Time, final bool, now time.Time, cause error) {
	ctx, cancel := s.settleContext()
	defer cancel()
	var claimed *time.Time
	if !final {
		claimed = &next
	}
	back, err := s.store.Release(ctx, sc.ID, claimed, due)
	if err != nil {
		s.log.Error("schedule: hand back an interrupted fire: "+err.Error(),
			zap.String("schedule_id", sc.ID))
	}
	reason := "the server stopped while this fire was starting its run, and the schedule changed " +
		"before the fire could be handed back, so this occurrence did not run: " + cause.Error()
	if back {
		reason = "the server stopped while this fire was starting its run, so the occurrence was " +
			"handed back and fires when a scheduler next checks: " + cause.Error()
	}
	s.log.Warn("schedule: "+reason, zap.String("schedule_id", sc.ID))
	if err := s.store.RecordFire(ctx, sc.ID, now, "", util.Clip(reason, maxFailure)); err != nil {
		s.log.Error("schedule: record fire: "+err.Error(), zap.String("schedule_id", sc.ID))
	}
}

// claim wins a due schedule's row for this instance: it advances the next fire time to next, or,
// for the last occurrence of a bounded recurrence, clears it. Either way the write is a
// compare-and-set on the next fire time this tick read, so only one instance of a highly available
// pair wins it. The write runs on the settle context, since a claim canceled midway may have landed
// with nobody to fire what it took.
func (s *Scheduler) claim(sc *Schedule, next time.Time, final bool) (bool, error) {
	ctx, cancel := s.settleContext()
	defer cancel()
	if final {
		return s.store.ClaimFinal(ctx, sc.ID, *sc.NextRunAt)
	}
	return s.store.ClaimDue(ctx, sc.ID, *sc.NextRunAt, next)
}

// overlaps returns the id of a run the schedule fired that is still going, or the empty string.
//
// A read error counts as not overlapping. Refusing to fire because the run store could not be read
// would turn a transient database problem into silently skipped automation, which is the failure
// nobody notices; firing may at worst produce the overlap this avoids, which is visible.
func (s *Scheduler) overlaps(sc *Schedule) string {
	if s.runActive == nil {
		return ""
	}
	waiting, err := s.runActive(s.ctx, sc.ID, sc.LastRunID)
	if err != nil {
		if s.ctx.Err() == nil {
			s.log.Error("schedule: check previous run: "+err.Error(),
				zap.String("schedule_id", sc.ID), zap.String("run_id", sc.LastRunID))
		}
		return ""
	}
	return waiting
}

// fireRecord is the canonical body a schedule's fire entry commits: which schedule fired and what
// it was configured to launch at that moment.
type fireRecord struct {
	// ScheduleID and Name identify the schedule.
	ScheduleID string `json:"schedule_id"`
	Name       string `json:"name,omitempty"`
	// TemplateID, Playbook, Inventory, and Shards say what the fire launches.
	TemplateID string `json:"template_id,omitempty"`
	Playbook   string `json:"playbook,omitempty"`
	Inventory  string `json:"inventory,omitempty"`
	Shards     int    `json:"shards,omitempty"`
	// Steps counts a pipeline schedule's declared steps.
	Steps int `json:"steps,omitempty"`
}

// recordFireEntry appends the chain entry for a fire and returns a context carrying its receipt, so
// the run the fire creates is tied to the record of what launched it. It fails closed: a fire that
// cannot be recorded is skipped rather than performed silently, the same rule the API gate applies
// to every mutation. Without a configured chain the context is returned unchanged.
func (s *Scheduler) recordFireEntry(ctx context.Context, sc *Schedule) (context.Context, error) {
	if s.audits == nil {
		return ctx, nil
	}
	body, err := json.Marshal(fireRecord{
		ScheduleID: sc.ID, Name: sc.Name, TemplateID: sc.TemplateID,
		Playbook: sc.Playbook, Inventory: sc.Inventory, Shards: sc.Shards, Steps: len(sc.Steps),
	})
	if err != nil {
		return ctx, err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return ctx, err
	}
	entry := &audit.Entry{
		ID:    audit.NewID(),
		Actor: "system:scheduler", ActorType: "system",
		Method: audit.MethodSchedule, Path: "/schedules/" + sc.ID + "/fired",
		ContentDigest: digest, Nonce: nonce,
	}
	if err := s.audits.Append(ctx, entry); err != nil {
		return ctx, fmt.Errorf("refused: the fire could not be recorded in the audit trail: %w", err)
	}
	return run.WithAuditReceipt(ctx, audit.Receipt(entry)), nil
}

// refusalRecord is the canonical body a refused fire's entry commits: which schedule came due, the
// template it would have fired, and why it fired nothing.
type refusalRecord struct {
	// ScheduleID and Name identify the schedule.
	ScheduleID string `json:"schedule_id"`
	Name       string `json:"name,omitempty"`
	// TemplateID is the template the schedule fires.
	TemplateID string `json:"template_id"`
	// Unanswered are the required survey questions nobody was present to answer.
	Unanswered []string `json:"unanswered"`
	// Reason is the refusal as the schedule records it.
	Reason string `json:"reason"`
}

// refuseUnanswered records that a fire was refused because the template's survey has a required
// question with no usable default, and returns the reason the schedule records as its last error.
//
// The refusal is its own chain entry, naming the questions in its path, so the trail shows the
// schedule came due and refused instead of a fire that ran nothing. The run it would have created
// never exists, so a chain that cannot record the refusal changes nothing about the outcome, and
// the reason says the record is missing.
func (s *Scheduler) refuseUnanswered(ctx context.Context, sc *Schedule, t *template.Template, cause error) error {
	reason := t.RefuseUnattended("scheduled fire", cause)
	if s.audits == nil {
		return reason
	}
	vars := template.UnansweredVars(cause)
	body, err := json.Marshal(refusalRecord{
		ScheduleID: sc.ID, Name: sc.Name, TemplateID: t.ID, Unanswered: vars, Reason: reason.Error(),
	})
	if err != nil {
		return reason
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return reason
	}
	entry := &audit.Entry{
		ID:    audit.NewID(),
		Actor: "system:scheduler", ActorType: "system",
		Method:        audit.MethodSchedule,
		Path:          "/schedules/" + sc.ID + "/refused/survey/" + strings.Join(vars, ","),
		ContentDigest: digest, Nonce: nonce,
	}
	if err := s.audits.Append(ctx, entry); err != nil {
		return fmt.Errorf("%w (the refusal could not be recorded in the audit trail: %v)", reason, err)
	}
	return reason
}

// loadTemplate returns the template a schedule fires, nil for a schedule that names none.
func (s *Scheduler) loadTemplate(ctx context.Context, sc *Schedule) (*template.Template, error) {
	if sc.TemplateID == "" {
		return nil, nil
	}
	if s.templates == nil {
		return nil, fmt.Errorf("schedule %s names a template but templates are not configured", sc.ID)
	}
	t, err := s.templates.Get(ctx, sc.TemplateID)
	if err != nil {
		return nil, fmt.Errorf("schedule %s: %w", sc.ID, err)
	}
	return t, nil
}

// fire submits the schedule's target and returns the created run id.
//
// A template's survey is resolved before the fire is recorded. Nobody is present to answer it, so
// every question takes its default, and a required question without one refuses the fire: the
// refusal is recorded in place of a fire and becomes the schedule's last error, and no run exists.
// A template that will not load is still reported after the fire entry, as it always was.
//
// The run carries an idempotency key naming the schedule and the occurrence it fires, the next fire
// time sc was read with, so whichever fire of one occurrence reaches the store second, such as the
// one that takes up an occurrence a stop handed back, finds the run the first one made rather than
// starting another.
func (s *Scheduler) fire(ctx context.Context, sc *Schedule) (string, error) {
	t, loadErr := s.loadTemplate(ctx, sc)
	var survey []run.SubmitOption
	if t != nil {
		opts, err := t.UnattendedOptions()
		if err != nil {
			return "", s.refuseUnanswered(ctx, sc, t, err)
		}
		survey = opts
	}
	ctx, err := s.recordFireEntry(ctx, sc)
	if err != nil {
		return "", err
	}
	if loadErr != nil {
		return "", loadErr
	}
	occurrence := ""
	if sc.NextRunAt != nil {
		occurrence = run.ScheduleKey(sc.ID, *sc.NextRunAt)
	}
	key := run.WithIdempotencyKey(occurrence)
	// The run belongs to the organization whose schedule fired it. Nothing else can supply that: the
	// tick loop carries no actor for the submit path to infer an org from, and an inline schedule's run
	// names no stored object for grants to reach, so an unstamped run is ownerless, which under strict
	// grants means denied to every non-admin. The tenant that owns the schedule could see the schedule
	// and none of the runs it produced.
	base := []run.SubmitOption{run.WithSource("schedule", sc.ID), run.WithOrgID(sc.OrgID), key}

	var created *run.Run
	switch {
	case t != nil:
		created, err = s.fireTemplate(ctx, sc, t, append(survey, key))
	case len(sc.Steps) > 0:
		created, err = s.submitter.SubmitPipeline(ctx, sc.Name, sc.Inventory, sc.Steps, base...)
	case sc.Shards >= 2:
		created, err = s.submitter.SubmitSplit(ctx, sc.Playbook, sc.Inventory, sc.Shards, base...)
	default:
		created, err = s.submitter.Submit(ctx, sc.Playbook, sc.Inventory, base...)
	}
	if err != nil {
		return "", err
	}
	s.log.Info("schedule fired",
		zap.String("schedule_id", sc.ID), zap.String("run_id", created.ID))
	return created.ID, nil
}

// fireTemplate launches the schedule's stored template t with its full preset, project,
// credentials, extra vars, and shards, plus extra, the options UnattendedOptions resolved for it
// and the fire's idempotency key.
func (s *Scheduler) fireTemplate(ctx context.Context, sc *Schedule, t *template.Template,
	extra []run.SubmitOption) (*run.Run, error) {
	// The schedule's own organization owns the run, and the template's stands in when an older schedule
	// carries none, so a run fired from a template is never left ownerless either.
	owner := sc.OrgID
	if owner == "" {
		owner = t.OrgID
	}
	opts := append(append(t.LaunchOptions(), extra...),
		run.WithSource("schedule", sc.ID), run.WithOrgID(owner))
	switch {
	case len(t.Steps) > 0:
		// A scheduled workflow template fires its graph, the same as an on-demand launch does.
		return s.submitter.SubmitPipeline(ctx, t.Name, t.Inventory, t.Steps, opts...)
	case t.Shards >= 2:
		return s.submitter.SubmitSplit(ctx, t.Playbook, t.Inventory, t.Shards, opts...)
	default:
		return s.submitter.Submit(ctx, t.Playbook, t.Inventory, opts...)
	}
}
