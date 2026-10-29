// Package relay is the seam for SwitchTender's mesh relay, the protocol that lets a worker in an
// isolated network segment dial the control node over one outbound connection and execute leased
// runs with no path to the shared database.
//
// Both ends of the protocol are Team code in the paid module: the worker's wire client and the
// control node's worker endpoints. This package holds only the contracts the control node
// satisfies, the Engine interface the paid module implements, and the registry slot it registers
// into. A Community build has no relay code, so its Engine refuses naming Team.
package relay

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/paid"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// Announcer tells the notification channels about a run: that it finished, or that it is held for a
// person. The dispatcher satisfies it.
//
// A worker holds no notification channels, and one in a segment that cannot reach a chat service
// could not use them anyway, so the control node announces the runs workers execute at the moment
// it records the change.
type Announcer interface {
	// Announce tells the channels about r.
	Announce(r *run.Run)
}

// AnnouncerFunc adapts a function to an Announcer.
type AnnouncerFunc func(r *run.Run)

// Announce calls f.
func (f AnnouncerFunc) Announce(r *run.Run) { f(r) }

// SecretOpener opens one claimed run's secrets on the control node, for the relay to seal to the
// worker pool that claimed it. The dispatcher satisfies it, so the set opened is exactly the set an
// executor with database access would open for the same run.
type SecretOpener interface {
	// NeedsSecrets reports whether r carries any secret its executor would open.
	NeedsSecrets(ctx context.Context, r *run.Run) bool
	// OpenSecrets opens them. It returns the payload and a release for anything the opening minted,
	// nil when nothing was, which the relay runs when the claim ends.
	OpenSecrets(ctx context.Context, r *run.Run) (*handoff.Payload, func(), error)
}

// PlanSealer seals the plan file a worker's plan saved, with the control node's key, so the apply
// proposed from it carries the plan sealed at rest. The dispatcher satisfies it.
type PlanSealer interface {
	// SealPlanFile seals plan for storage on the apply that carries it out.
	SealPlanFile(plan []byte) (string, error)
}

// PresenceRecorder records which workers are polling for work. The attention store satisfies it.
type PresenceRecorder interface {
	// NoteWorker records that owner is polling for work, serving queues with slots runs at once.
	NoteWorker(ctx context.Context, owner string, queues []string, slots int) error
	// TouchWorker records that owner is still reporting, leaving what it serves alone.
	TouchWorker(ctx context.Context, owner string) error
}

// MaxPlanFileBytes is the largest plan file a relay worker can hand the control node, 19 MiB. The
// propose request carries the plan file base64 encoded, and the control node accepts at most 26 MiB
// on that route, which leaves room for this much plan file and the request around it.
const MaxPlanFileBytes = 19 << 20

// Pools is the worker pool configuration the control node serves: a validated pool file, or the
// single unconfined pool a lone worker token makes. Only the Engine that built it can serve it.
type Pools interface {
	// DeliveryKeyIDs returns the delivery key id each pool registered, by pool name.
	DeliveryKeyIDs() map[string]string
}

// HandlerConfig is what the control node's worker endpoints are built from.
type HandlerConfig struct {
	// Store is the run store the endpoints serve. It must implement run.SummaryAppender.
	Store run.Store
	// Pools authenticate workers and confine each token to its queues.
	Pools Pools
	// Log receives the endpoints' logs; nil is a no-op.
	Log *zap.Logger
	// Policies are the approval policies a worker reads, nil when none are configured.
	Policies policy.Store
	// Audits records what workers do, nil when nothing is audited.
	Audits audit.Store
	// Announcer tells the channels about the runs workers finish, nil when none.
	Announcer Announcer
	// SecretOpener opens a claimed run's secrets for sealed delivery, nil to refuse every delivery.
	SecretOpener SecretOpener
	// PlanSealer seals a worker's plan file onto the apply it proposes, nil to accept none.
	PlanSealer PlanSealer
	// Binder holds each leased run to the control node's seal, nil when no binding key is held.
	Binder *binding.Verifier
	// Admit refuses a proposed apply the control node has no room to record, nil to check nothing.
	Admit func() error
	// Presence records the workers that reach the control node, nil to record none.
	Presence PresenceRecorder
}

// DialConfig is what a relay worker dials its control node with.
type DialConfig struct {
	// Server is the control node's base URL.
	Server string
	// Token is the worker token the control node's pools know.
	Token string `json:"-"`
	// Client carries the requests.
	Client *http.Client
	// Keys open the secrets the control node seals to this worker's pool, nil or empty for none.
	Keys *handoff.KeyRing
}

// Worker is what a relay worker executes against, all over one connection to the control node.
type Worker struct {
	// Store is the run store the worker's dispatcher claims from and reports to.
	Store run.Store
	// Policies are the approval policies the control node serves.
	Policies policy.Store
	// Delivery opens the secrets sealed to this worker.
	Delivery dispatch.SecretDelivery
}

// Engine is the mesh relay, both ends. The paid module registers the implementation; the Community
// stub refuses every method naming Team.
type Engine interface {
	// LoadPools reads and validates the worker pool file at path.
	LoadPools(path string) (Pools, error)
	// SinglePool returns the one unconfined pool a lone worker token makes.
	SinglePool(token string) (Pools, error)
	// Handler returns the control node's worker endpoints.
	Handler(cfg HandlerConfig) (http.Handler, error)
	// Dial connects a relay worker to its control node.
	Dial(cfg DialConfig) (*Worker, error)
}

// stubEngine is the Community relay: there is no relay code in the build, so it refuses naming
// Team.
type stubEngine struct{}

// LoadPools refuses.
func (stubEngine) LoadPools(string) (Pools, error) {
	return nil, paid.Requires(license.FeatureWorkers)
}

// SinglePool refuses.
func (stubEngine) SinglePool(string) (Pools, error) {
	return nil, paid.Requires(license.FeatureWorkers)
}

// Handler refuses.
func (stubEngine) Handler(HandlerConfig) (http.Handler, error) {
	return nil, paid.Requires(license.FeatureWorkers)
}

// Dial refuses.
func (stubEngine) Dial(DialConfig) (*Worker, error) {
	return nil, paid.Requires(license.FeatureWorkers)
}

// engineSlot holds the relay engine, the Community stub until the paid module registers one.
var engineSlot = paid.NewSlot[Engine]("relay engine", stubEngine{})

// RegisterEngine registers the paid relay engine. A second registration panics.
func RegisterEngine(e Engine) { engineSlot.Set(e) }

// CurrentEngine returns the relay engine, the Community stub when none is registered.
func CurrentEngine() Engine { return engineSlot.Get() }

// Registered reports whether a paid relay engine is registered.
func Registered() bool { return engineSlot.Registered() }
