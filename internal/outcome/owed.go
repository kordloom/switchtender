package outcome

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// CommitOwed commits a finished run's outcome to the chain through Commit and then settles what the
// store owes for it, so every process that finishes a run, and the janitor that finishes what they
// could not, commits an outcome the same way and exactly once.
//
// The chain keeps a run to one outcome entry, so an outcome some other process already committed is
// refused with audit.ErrOutcomeRecorded, and that is success here: the outcome is on the chain.
// When the commit fails the run stays owed, and the janitor commits it once the chain takes it. A
// settle that fails after the outcome reached the chain is the same case the other way round: the
// janitor finds it owed, the chain refuses a second entry, and the settle runs then.
func CommitOwed(ctx context.Context, audits audit.Store, store run.Store, r *run.Run, committer string,
	now func() time.Time) error {
	if err := Commit(ctx, audits, store, r, committer, now); err != nil &&
		!errors.Is(err, audit.ErrOutcomeRecorded) {
		return err
	}
	if err := store.SettleOutcome(ctx, r.ID); err != nil {
		return fmt.Errorf("the outcome of run %s is on the chain and is still marked owed: %w",
			r.ID, err)
	}
	return nil
}
