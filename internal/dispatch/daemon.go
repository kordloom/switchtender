package dispatch

import (
	"context"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/paid"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// DaemonConfig is what the worker daemon starts from.
type DaemonConfig struct {
	// Open opens the store the daemon claims from and returns it with the options that wire it and
	// a func that releases it. A Community build refuses before calling it, so a refused worker
	// touches no database and dials no control node.
	Open func() (run.Store, []Option, func(), error)
	// Runner executes the runs the daemon claims.
	Runner roundhouse.Runner
	// Name is the owner stamped on the runs it claims, host and pid when empty.
	Name string
	// Queues are the named queues it serves, the default queue when empty.
	Queues []string
	// Source names where it claims from for its start line, with no credential in it.
	Source string
	// Log records the daemon's activity.
	Log *zap.Logger
}

// Daemon is the worker daemon: a dispatcher that claims runs under its own owner and queues and
// rereads the license before every claim. A Community build refuses naming Team.
type Daemon interface {
	// Run starts the daemon and returns when ctx is done.
	Run(ctx context.Context, cfg DaemonConfig) error
}

// stubDaemon is the Community worker daemon: it refuses naming Team before opening anything.
type stubDaemon struct{}

// Run refuses.
func (stubDaemon) Run(context.Context, DaemonConfig) error {
	return paid.Requires(license.FeatureWorkers)
}

// daemonSlot holds the worker daemon, the stub until the paid module registers it.
var daemonSlot = paid.NewSlot[Daemon]("worker daemon", stubDaemon{}, license.FeatureWorkers)

// RegisterDaemon installs the paid worker daemon. It panics on a second call.
func RegisterDaemon(d Daemon) { daemonSlot.Set(d) }

// CurrentDaemon returns the worker daemon, the Community stub when none is registered.
func CurrentDaemon() Daemon { return daemonSlot.Get() }
