package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// SkipBadgeFires is how many fires in a row a schedule must have skipped before it is flagged: the
// schedules list shows a badge and the doctor warns. One skip can be ordinary, such as a smart
// inventory over the hosts waiting for a patch, which is sometimes empty. Three in a row is a
// schedule that has stopped reaching anything.
const SkipBadgeFires = 3

// SkipNoHosts is the reason recorded for a fire whose composed inventory resolved to no hosts.
const SkipNoHosts = "no hosts matched"

// SkipNotifier tells the notification targets attached to a schedule that one of its fires was
// skipped. The dispatcher satisfies it.
type SkipNotifier interface {
	// AnnounceSkip delivers the run-shaped notice of a skipped fire, which is never stored.
	AnnounceSkip(r *run.Run)
}

// SkipNotifierFunc adapts a function to a SkipNotifier.
type SkipNotifierFunc func(r *run.Run)

// AnnounceSkip calls f.
func (f SkipNotifierFunc) AnnounceSkip(r *run.Run) { f(r) }

// WithSkipNotifier tells each schedule's attached notification targets when one of its fires is
// skipped.
func WithSkipNotifier(n SkipNotifier) SchedulerOption {
	return func(s *Scheduler) { s.skips = n }
}

// skipped reports whether a fire's error skips the fire rather than failing it: its composed
// inventory resolved to no hosts, so there was nothing to run against.
func skipped(err error) bool {
	return errors.Is(err, inventory.ErrNoHosts)
}

// skipRecord is the canonical body a skip's chain entry commits: which schedule skipped, what it
// was set to reach, when it fired, and why it started nothing.
type skipRecord struct {
	// ScheduleID identifies the schedule.
	ScheduleID string `json:"schedule_id"`
	// Name is the schedule's name.
	Name string `json:"name,omitempty"`
	// TemplateID is the template the fire launched.
	TemplateID string `json:"template_id,omitempty"`
	// InventoryID is the composed inventory that resolved to no hosts.
	InventoryID string `json:"inventory_id,omitempty"`
	// FiredAt is when the fire came due, in UTC.
	FiredAt string `json:"fired_at"`
	// Reason is why the fire started nothing.
	Reason string `json:"reason"`
}

// skip records a fire whose inventory matched no hosts: its own chain entry, the skip on the
// schedule, an info line in the log, and a notice to the targets attached to the schedule. It is
// never recorded as a failure, because nothing failed. The one exception is a skip the chain will
// not take: that fire is recorded as failed with the reason, so no skip exists without its
// evidence. The records are written on the settle context, so a stop that lands after the fire
// cannot leave the claim with no record of how it ended.
func (s *Scheduler) skip(sc *Schedule, now time.Time) {
	ctx, cancel := s.settleContext()
	defer cancel()
	inventoryID, owner := s.fireTarget(ctx, sc)
	if err := s.recordSkipEntry(ctx, sc, inventoryID, now); err != nil {
		s.log.Error("schedule: record skip: "+err.Error(), zap.String("schedule_id", sc.ID))
		failure := util.Clip(err.Error(), maxFailure)
		if rerr := s.store.RecordFire(ctx, sc.ID, now, "", failure); rerr != nil {
			s.log.Error("schedule: record fire: "+rerr.Error(), zap.String("schedule_id", sc.ID))
		}
		return
	}
	if err := s.store.RecordSkip(ctx, sc.ID, now, SkipNoHosts); err != nil {
		s.log.Error("schedule: record skip: "+err.Error(), zap.String("schedule_id", sc.ID))
	}
	s.log.Info("schedule fire skipped: "+SkipNoHosts,
		zap.String("schedule_id", sc.ID), zap.String("inventory_id", inventoryID))
	if s.skips != nil {
		s.skips.AnnounceSkip(skipNotice(sc, inventoryID, owner, now))
	}
}

// fireTarget returns the stored inventory a schedule's fire targets and the organization its run
// would have belonged to, read from the template it fires. An inline schedule names an inventory
// path rather than a stored one, so it has no inventory id here.
func (s *Scheduler) fireTarget(ctx context.Context, sc *Schedule) (inventoryID, owner string) {
	owner = sc.OrgID
	if sc.TemplateID == "" || s.templates == nil {
		return "", owner
	}
	t, err := s.templates.Get(ctx, sc.TemplateID)
	if err != nil {
		return "", owner
	}
	if owner == "" {
		owner = t.OrgID
	}
	return t.InventoryID, owner
}

// recordSkipEntry appends the chain entry for a skipped fire. Without a configured chain there is
// nothing to append to and it succeeds.
func (s *Scheduler) recordSkipEntry(ctx context.Context, sc *Schedule, inventoryID string, at time.Time) error {
	if s.audits == nil {
		return nil
	}
	body, err := json.Marshal(skipRecord{
		ScheduleID: sc.ID, Name: sc.Name, TemplateID: sc.TemplateID, InventoryID: inventoryID,
		FiredAt: at.UTC().Format(time.RFC3339Nano), Reason: SkipNoHosts,
	})
	if err != nil {
		return err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return err
	}
	entry := &audit.Entry{
		ID:    audit.NewID(),
		Actor: "system:scheduler", ActorType: "system",
		Method: audit.MethodSchedule, Path: "/schedules/" + sc.ID + "/skipped",
		ContentDigest: digest, Nonce: nonce,
	}
	if err := s.audits.Append(ctx, entry); err != nil {
		return fmt.Errorf("refused: the skip could not be recorded in the audit trail: %w", err)
	}
	return nil
}

// skipNotice builds the run-shaped notice a skipped fire is announced with. It is never stored. It
// names the schedule as its source, so the targets attached to the schedule hear it the way they
// hear the runs it fires, and carries the schedule's name as a label for the message to show.
func skipNotice(sc *Schedule, inventoryID, owner string, at time.Time) *run.Run {
	name := sc.Name
	if name == "" {
		name = sc.ID
	}
	return &run.Run{
		Kind: run.KindSkippedFire, Source: "schedule", SourceID: sc.ID, TemplateID: sc.TemplateID,
		InventoryID: inventoryID, OrgID: owner, CreatedAt: at, Warning: SkipNoHosts,
		Actor: "system:scheduler", ActorType: "system",
		Labels: map[string]string{"schedule": name},
	}
}
