package relay

import (
	"context"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// Client is a run.Store a relay worker runs against. It forwards the execution-path methods to the
// Transport and returns ErrUnsupported for the query and analytics reads a worker never performs, so
// it drops straight into dispatch.New in place of a database-backed store.
type Client struct {
	// t carries the execution-path calls to the control node.
	t Transport
}

// NewClient returns a run.Store backed by the Transport, ready to hand to dispatch.New in a worker.
func NewClient(t Transport) *Client {
	return &Client{t: t}
}

// compile-time proof that a Client is a full run.Store.
var _ run.Store = (*Client)(nil)

// Claim leases work from the control node.
func (c *Client) Claim(ctx context.Context, owner string, queues []string) (*run.Run, error) {
	return c.t.Claim(ctx, owner, queues)
}

// Heartbeat renews the worker's lease on the control node.
func (c *Client) Heartbeat(ctx context.Context, id, owner string) error {
	return c.t.Heartbeat(ctx, id, owner)
}

// Get reads a run, and with it the cancel flag, from the control node.
func (c *Client) Get(ctx context.Context, id string) (*run.Run, error) {
	return c.t.Get(ctx, id)
}

// Save writes a run's status transitions back to the control node.
func (c *Client) Save(ctx context.Context, r *run.Run) error {
	return c.t.Save(ctx, r)
}

// AppendLog streams captured output to the control node.
func (c *Client) AppendLog(ctx context.Context, id string, p []byte) error {
	return c.t.AppendLog(ctx, id, p)
}

// AppendEvents streams structured events to the control node.
func (c *Client) AppendEvents(ctx context.Context, id string, events []event.Event) error {
	return c.t.AppendEvents(ctx, id, events)
}

// SaveHostSummary records a run's per-host outcomes on the control node.
func (c *Client) SaveHostSummary(ctx context.Context, runID string, summaries []run.HostSummary) error {
	return c.t.SaveHostSummary(ctx, runID, summaries)
}

// SaveHostFacts records the system facts a run gathered per host on the control node.
func (c *Client) SaveHostFacts(ctx context.Context, runID string, facts []run.HostFacts) error {
	return c.t.SaveHostFacts(ctx, runID, facts)
}

// SaveTaskSummary records a run's per-task durations on the control node.
func (c *Client) SaveTaskSummary(ctx context.Context, runID string, summaries []run.TaskSummary) error {
	return c.t.SaveTaskSummary(ctx, runID, summaries)
}

// The remaining run.Store methods are control-node queries and analytics a worker never calls, so a
// relay Client refuses them rather than pretending to serve them across the wire.

// TransitionStatusAndClaim is a control-node operation. Approving a held run happens where the
// policy and the approver are, never on a worker.
func (c *Client) TransitionStatusAndClaim(context.Context, string, run.Status, run.Status,
	string) (bool, error) {
	return false, ErrUnsupported
}

// ByIdempotencyKey is a control-node query and is not served to workers.
func (c *Client) ByIdempotencyKey(context.Context, string) (*run.Run, error) {
	return nil, ErrUnsupported
}

// List is a control-node query and is not served to workers.
func (c *Client) List(context.Context) ([]*run.Run, error) { return nil, ErrUnsupported }

// ListPage is a control-node query and is not served to workers.
func (c *Client) ListPage(context.Context, run.ListFilter, int, int) ([]*run.Run, error) {
	return nil, ErrUnsupported
}

// RunStatusCounts is a control-node query and is not served to workers.
func (c *Client) RunStatusCounts(context.Context) (map[run.Status]int, error) {
	return nil, ErrUnsupported
}

// RunTimings is a control-node read and is not served to workers.
func (c *Client) RunTimings(context.Context, int) ([]run.RunTiming, error) {
	return nil, ErrUnsupported
}

// Shards is a control-node query and is not served to workers.
func (c *Client) Shards(context.Context, string) ([]*run.Run, error) { return nil, ErrUnsupported }

// Steps is a control-node query and is not served to workers.
func (c *Client) Steps(context.Context, string) ([]*run.Run, error) { return nil, ErrUnsupported }

// NonTerminal is a control-node query and is not served to workers.
func (c *Client) NonTerminal(context.Context) ([]*run.Run, error) { return nil, ErrUnsupported }

// ReclaimStale is a control-node sweep and is not served to workers.
func (c *Client) ReclaimStale(context.Context, time.Duration) (int, error) {
	return 0, ErrUnsupported
}

// RequestCancel is issued by the control node's API, not a worker.
func (c *Client) RequestCancel(context.Context, string) error { return ErrUnsupported }

// TransitionStatus is a control-node operation and is not served to workers.
func (c *Client) TransitionStatus(context.Context, string, run.Status, run.Status) (bool, error) {
	return false, ErrUnsupported
}

// StampApprovedSpec is a control-node write and is not served to workers: decisions are made where
// the approver is, never on the far side of the relay.
func (c *Client) StampApprovedSpec(context.Context, string, string) error {
	return ErrUnsupported
}

// FinalizeRunning is a control-node write and is not served to workers. A relay worker reports how
// a run ended through Save, and the control node applies that report to the run it holds.
func (c *Client) FinalizeRunning(context.Context, string, run.Finalization) (bool, error) {
	return false, ErrUnsupported
}

// Workers is a control-node query and is not served to workers.
func (c *Client) Workers(context.Context) ([]run.WorkerInfo, error) { return nil, ErrUnsupported }

// FleetHealth is a control-node analytic and is not served to workers.
func (c *Client) FleetHealth(context.Context, int) ([]run.HostHealth, error) {
	return nil, ErrUnsupported
}

// DriftStatus is a control-node analytic and is not served to workers.
func (c *Client) DriftStatus(context.Context) ([]run.HostDrift, error) { return nil, ErrUnsupported }

// HostCosts is a control-node analytic and is not served to workers.
func (c *Client) HostCosts(context.Context, int) (map[string]float64, error) {
	return nil, ErrUnsupported
}

// HostHistory is a control-node query and is not served to workers.
func (c *Client) HostHistory(context.Context, string, int) ([]run.HostSummary, error) {
	return nil, ErrUnsupported
}

// RunHostSummaries is a control-node read and is not served to workers.
func (c *Client) RunHostSummaries(context.Context, string) ([]run.HostSummary, error) {
	return nil, ErrUnsupported
}

// RunTaskSummaries is a control-node read and is not served to workers.
func (c *Client) RunTaskSummaries(context.Context, string) ([]run.TaskSummary, error) {
	return nil, ErrUnsupported
}

// HostFactsFor is a control-node read and is not served to workers.
func (c *Client) HostFactsFor(context.Context, string) (*run.HostFacts, error) {
	return nil, ErrUnsupported
}

// TaskTrends is a control-node analytic and is not served to workers.
func (c *Client) TaskTrends(context.Context, int) ([]run.TaskTrend, error) {
	return nil, ErrUnsupported
}

// Log is a control-node query and is not served to workers.
func (c *Client) Log(context.Context, string) ([]byte, error) { return nil, ErrUnsupported }

// LogAfter is a control-node query and is not served to workers.
func (c *Client) LogAfter(context.Context, string, int64, int) ([]run.LogChunk, error) {
	return nil, ErrUnsupported
}

// LastLogSeq is a control-node query and is not served to workers.
func (c *Client) LastLogSeq(context.Context, string) (int64, error) { return 0, ErrUnsupported }

// CancelPending is a control-node mutation and is not served to workers.
func (c *Client) CancelPending(context.Context, string) (bool, error) {
	return false, ErrUnsupported
}

// Events is a control-node query and is not served to workers.
func (c *Client) Events(context.Context, string) ([]event.Event, error) { return nil, ErrUnsupported }

// EventsAfter is a control-node query and is not served to workers.
func (c *Client) EventsAfter(context.Context, string, int64, int) ([]event.Event, error) {
	return nil, ErrUnsupported
}

// LastEventSeq is a control-node query and is not served to workers.
func (c *Client) LastEventSeq(context.Context, string) (int64, error) { return 0, ErrUnsupported }

// PurgeEventsBefore is a control-node retention sweep and is not served to workers.
func (c *Client) PurgeEventsBefore(context.Context, time.Time) (int, error) {
	return 0, ErrUnsupported
}

// PurgeRunsBefore is a control-node retention sweep and is not served to workers.
func (c *Client) PurgeRunsBefore(context.Context, time.Time) (int, error) { return 0, ErrUnsupported }

// TrimSummaries is a control-node retention sweep and is not served to workers.
func (c *Client) TrimSummaries(context.Context, int) (int, error) { return 0, ErrUnsupported }
