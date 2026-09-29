package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// DefaultInterval is how often the scheduler checks for due schedules when none is configured.
const DefaultInterval = 15 * time.Second

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
	// audits records each fire as a chain entry before the run exists, nil when no trail is kept.
	// With it, a scheduled run carries a creation receipt like any other and can be receipted.
	audits audit.Store
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

// Start begins the scheduler loop in a background goroutine.
func (s *Scheduler) Start() {
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
}

// Close stops the scheduler loop and waits for it to exit.
func (s *Scheduler) Close() {
	s.cancel()
	<-s.done
}

// tick fires every schedule due at now and advances its next run time.
func (s *Scheduler) tick(now time.Time) {
	schedules, err := s.store.List(s.ctx)
	if err != nil {
		s.log.Error("schedule: list: " + err.Error())
		return
	}
	for _, sc := range schedules {
		if !sc.Enabled || sc.NextRunAt == nil || sc.NextRunAt.After(now) {
			continue
		}

		next, err := sc.NextFire(now)
		if err != nil {
			s.log.Error("schedule: next fire: "+err.Error(), zap.String("schedule_id", sc.ID))
			continue
		}
		// Win the row before firing so concurrent scheduler instances never double-launch.
		won, err := s.store.ClaimDue(s.ctx, sc.ID, *sc.NextRunAt, next)
		if err != nil {
			s.log.Error("schedule: claim due: "+err.Error(), zap.String("schedule_id", sc.ID))
			continue
		}
		if !won {
			continue
		}

		runID, err := s.fire(s.ctx, sc)
		if err != nil {
			s.log.Error("schedule: fire: "+err.Error(), zap.String("schedule_id", sc.ID))
		}
		// Only what the fire owns is written back. sc came from the List above, so it is a snapshot
		// taken before the run and writing it whole reverted anything an operator changed meanwhile:
		// a disable came back enabled, an edit was rolled back, and a delete was re-inserted as a
		// live schedule that kept firing. NextRunAt is deliberately not written either, because
		// ClaimDue already advanced it and rewriting it here is what reverted an edited cron.
		if err := s.store.RecordFire(s.ctx, sc.ID, now, runID); err != nil {
			s.log.Error("schedule: record fire: "+err.Error(), zap.String("schedule_id", sc.ID))
		}
	}
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
		ID: audit.NewID(), At: time.Now(),
		Actor: "system:scheduler", ActorType: "system",
		Method: audit.MethodSchedule, Path: "/schedules/" + sc.ID + "/fired",
		ContentDigest: digest, Nonce: nonce,
	}
	if err := s.audits.Append(ctx, entry); err != nil {
		return ctx, fmt.Errorf("refused: the fire could not be recorded in the audit trail: %w", err)
	}
	return run.WithAuditReceipt(ctx, audit.Receipt(entry)), nil
}

// fire submits the schedule's target and returns the created run id.
func (s *Scheduler) fire(ctx context.Context, sc *Schedule) (string, error) {
	ctx, err := s.recordFireEntry(ctx, sc)
	if err != nil {
		return "", err
	}
	// The run belongs to the organization whose schedule fired it. Nothing else can supply that: the
	// tick loop carries no actor for the submit path to infer an org from, and an inline schedule's run
	// names no stored object for grants to reach, so an unstamped run is ownerless, which under strict
	// grants means denied to every non-admin. The tenant that owns the schedule could see the schedule
	// and none of the runs it produced.
	base := []run.SubmitOption{run.WithSource("schedule", sc.ID), run.WithOrgID(sc.OrgID)}

	var created *run.Run
	switch {
	case sc.TemplateID != "":
		created, err = s.fireTemplate(ctx, sc)
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

// fireTemplate launches the schedule's stored template with its full preset: project,
// credentials, extra vars, and shards.
func (s *Scheduler) fireTemplate(ctx context.Context, sc *Schedule) (*run.Run, error) {
	if s.templates == nil {
		return nil, fmt.Errorf("schedule %s names a template but templates are not configured", sc.ID)
	}
	t, err := s.templates.Get(ctx, sc.TemplateID)
	if err != nil {
		return nil, fmt.Errorf("schedule %s: %w", sc.ID, err)
	}
	// The schedule's own organization owns the run, and the template's stands in when an older schedule
	// carries none, so a run fired from a template is never left ownerless either.
	owner := sc.OrgID
	if owner == "" {
		owner = t.OrgID
	}
	opts := append(t.LaunchOptions(),
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
