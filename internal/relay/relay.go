// Package relay is the foundation of SwitchTender's phase-1 mesh: a worker in an isolated network
// segment dials the control node over one outbound connection and executes leased runs, so it needs
// no path to the shared database and no inbound firewall rule.
//
// The design reuses the whole existing dispatcher unchanged. A worker builds a Client, which
// implements run.Store by forwarding the execution-path calls (claim, heartbeat, read, status,
// output, events, summaries) to a Transport, and hands it to dispatch.New like any other store. The
// Transport is the wire: today a Loopback that delegates in-process to a real run.Store, and next a
// gRPC client that carries the same calls to a relay service on the control node. Because a worker's
// dispatcher only exercises the execution path on the store it runs against, the Client returns
// ErrUnsupported for the query and analytics reads, which the control node and its API serve, not a
// worker. The remaining phase-1 work is a single gRPC Transport implementation and its server side.
package relay

import (
	"context"
	"errors"

	"github.com/dcadolph/switchtender/internal/event"
	"github.com/dcadolph/switchtender/internal/run"
)

// ErrUnsupported is returned by a relay Client for the run.Store methods a worker never calls: the
// query and analytics reads served by the control node and its API rather than the execution path.
var ErrUnsupported = errors.New("relay: method not served to workers")

// Transport carries a worker's execution-path store calls to the control node. It is the subset of
// run.Store a worker exercises while executing a leased run: claiming work, renewing the lease,
// reading the claimed run, and recording status, output, events, and summaries. The Loopback delegates
// it in-process; a gRPC transport carries it across the network in the rest of the mesh build.
type Transport interface {
	// Claim leases the oldest pending run the owner's queues serve.
	Claim(ctx context.Context, owner string, queues []string) (*run.Run, error)
	// Heartbeat renews the owner's lease on a run.
	Heartbeat(ctx context.Context, id, owner string) error
	// Get returns the run with the given id, so a worker reads the claimed run and its cancel flag.
	Get(ctx context.Context, id string) (*run.Run, error)
	// Save writes the run's status transitions back to the control node.
	Save(ctx context.Context, r *run.Run) error
	// AppendLog streams captured output to the control node.
	AppendLog(ctx context.Context, id string, p []byte) error
	// AppendEvents streams structured events to the control node.
	AppendEvents(ctx context.Context, id string, events []event.Event) error
	// SaveHostSummary records a run's per-host outcomes.
	SaveHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error
	// SaveTaskSummary records a run's per-task durations.
	SaveTaskSummary(ctx context.Context, runID string, summaries []run.TaskSummary) error
}

// Loopback adapts a run.Store into a Transport by delegating in-process, so a relay Client can run
// against a local store. The gRPC transport replaces it without touching the Client or the dispatcher.
func Loopback(store run.Store) Transport {
	return loopback{store: store}
}

// loopback is the in-process Transport backed by a run.Store.
type loopback struct {
	// store is the delegate that serves every call locally.
	store run.Store
}

// Claim delegates to the backing store.
func (l loopback) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	return l.store.Claim(ctx, owner, queues)
}

// Heartbeat delegates to the backing store.
func (l loopback) Heartbeat(ctx context.Context, id, owner string) error {
	return l.store.Heartbeat(ctx, id, owner)
}

// Get delegates to the backing store.
func (l loopback) Get(ctx context.Context, id string) (*run.Run, error) {
	return l.store.Get(ctx, id)
}

// Save delegates to the backing store.
func (l loopback) Save(ctx context.Context, r *run.Run) error {
	return l.store.Save(ctx, r)
}

// AppendLog delegates to the backing store.
func (l loopback) AppendLog(ctx context.Context, id string, p []byte) error {
	return l.store.AppendLog(ctx, id, p)
}

// AppendEvents delegates to the backing store.
func (l loopback) AppendEvents(ctx context.Context, id string, events []event.Event) error {
	return l.store.AppendEvents(ctx, id, events)
}

// SaveHostSummary delegates to the backing store.
func (l loopback) SaveHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error {
	return l.store.SaveHostSummary(ctx, runID, summaries)
}

// SaveTaskSummary delegates to the backing store.
func (l loopback) SaveTaskSummary(ctx context.Context, runID string, summaries []run.TaskSummary) error {
	return l.store.SaveTaskSummary(ctx, runID, summaries)
}
