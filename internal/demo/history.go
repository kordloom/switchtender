package demo

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// seedNightlyHistory runs the nightly audit the way its schedule fires it, once a night at the
// schedule's own time, across the seedHistoryDays before the run window. The runs list, the
// overview's two-week chart, and the schedule's own history then read like a fleet audited every
// night rather than one seeded in an afternoon. Each run is the audit playbook this host really
// executed, parked at the night it stands for by the seed clock, the clock that places every other
// seeded run, and each carries the schedule and the template that fired it.
func seedNightlyHistory(ctx context.Context, d Deps, auditPlay, inv string, ids seededIDs,
	log *zap.Logger) error {
	if d.Clock == nil || d.Schedules == nil {
		return nil
	}
	scheduleID := ids.Schedules["Nightly audit"]
	if scheduleID == "" {
		return nil
	}
	sc, err := d.Schedules.Get(ctx, scheduleID)
	if err != nil {
		log.Warn("demo: seed nightly history: " + err.Error())
		return nil
	}
	templateID := ids.Templates["Nightly audit"]
	at, err := sc.NextFire(d.Clock.Now())
	for ; err == nil && at.Before(d.Clock.windowAt); at, err = sc.NextFire(at) {
		d.Clock.jumpTo(at)
		r, serr := d.Submitter.Submit(ctx, auditPlay, inv,
			seedOpts(ctx, d, "schedule", scheduleID, "system:scheduler",
				map[string]string{"env": "prod", "team": "platform"},
				run.WithTemplate(templateID))...)
		if serr != nil {
			return fmt.Errorf("seed nightly audit: %w", serr)
		}
		settle(ctx, d, r.ID)
	}
	if err != nil {
		log.Warn("demo: seed nightly history: " + err.Error())
	}
	return nil
}
