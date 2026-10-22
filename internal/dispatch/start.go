package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// startTries bounds the appends a local start makes, the same number withRetries makes.
const startTries = 4

// RecordStart appends the START entry for r's attempt to audits and returns why r must not run,
// empty when it may. The caller has just moved r to running and runs nothing until this answers.
// The run's status is in no seal, so a writer can reset a finished row to pending; the entry's id
// is fixed by the run and its attempt, so the chain refuses the second start as a duplicate and the
// attempt that already ran runs nothing.
//
// A duplicate on the first try is that refusal. A duplicate after an earlier try failed may be the
// earlier try landing with its answer lost, which no read can tell from a genuine earlier start, so
// it fails closed as unverified, as does any other failure. tries bounds the appends, at least one.
// The error is for the caller's log; the reason is what the run records.
func RecordStart(ctx context.Context, audits audit.Store, r *run.Run, starter string,
	now func() time.Time, tries int) (string, error) {
	entry, err := outcome.StartEntry(r, starter, now)
	if err != nil {
		return startUnrecordedText(r), err
	}
	var failed error
	for try := range max(tries, 1) {
		if try > 0 {
			select {
			case <-time.After(time.Duration(try) * 75 * time.Millisecond):
			case <-ctx.Done():
				return startUnrecordedText(r), errors.Join(failed, ctx.Err())
			}
		}
		aerr := audits.Append(ctx, entry)
		switch {
		case aerr == nil:
			return "", nil
		case errors.Is(aerr, audit.ErrDuplicateID) && failed == nil:
			return StartRefusalText(r), aerr
		case errors.Is(aerr, audit.ErrDuplicateID):
			return startUnrecordedText(r), errors.Join(failed, aerr)
		}
		failed = aerr
	}
	return startUnrecordedText(r), failed
}

// StartRefusalText is the reason a run records when the chain already holds a start of its attempt.
func StartRefusalText(r *run.Run) string {
	return fmt.Sprintf("refused: run %s attempt %d already started once; a run that should run "+
		"again is retried, which gives it a new attempt", r.ID, r.Attempt)
}

// startUnrecordedText is the reason a run records when its start could not be recorded on the
// chain. It says "could not verify", never "refused": nothing was found tampered with.
func startUnrecordedText(r *run.Run) string {
	return fmt.Sprintf("%s run %s attempt %d: its start could not be recorded on the chain, so "+
		"it did not run", binding.ErrUnverified, r.ID, r.Attempt)
}

// startRefusal records r's start on a dispatcher with a binding key and a chain, and returns why r
// must not run, empty when it may. A dispatcher without either records nothing. The append is not
// ended by the run's cancellation, so a shutdown landing during it is never recorded as a start
// the chain could not take.
func (d *Dispatcher) startRefusal(ctx context.Context, r *run.Run) string {
	if d.binder == nil || d.audits == nil {
		return ""
	}
	reason, err := RecordStart(context.WithoutCancel(ctx), d.audits, r, "system:dispatcher", d.now,
		startTries)
	if reason != "" {
		d.log.Error("dispatch: record a run's start: "+err.Error(), zap.String("run_id", r.ID))
	}
	return reason
}
