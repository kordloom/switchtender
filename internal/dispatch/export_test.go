package dispatch

import (
	"context"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// SweepSecretLeases runs one sweep of the recorded secret leases as of now, for the tests outside
// this package that hold the stores this package's own tests cannot import.
func (d *Dispatcher) SweepSecretLeases(ctx context.Context, keeper run.SecretLeases, now time.Time) {
	d.sweepSecretLeases(ctx, keeper, now)
}

// MaterializeCredentials opens r's credentials onto spec the way execution does, for the tests
// outside this package, and returns the cleanup that hands back what the opening minted.
func (d *Dispatcher) MaterializeCredentials(ctx context.Context, r *run.Run,
	spec *roundhouse.Spec) (func(), error) {
	cleanup, _, err := d.materializeCredentials(ctx, r, spec)
	return cleanup, err
}
