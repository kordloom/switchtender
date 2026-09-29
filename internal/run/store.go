package run

import (
	"context"
	"time"

	"github.com/kordloom/switchtender/internal/event"
)

// orphanError is stamped on a child canceled because its split or pipeline parent was interrupted.
// Every store writes the same text, so a reclaimed run reads the same way whichever store holds it.
const orphanError = "canceled: the parent run was interrupted"

// OrphanError returns the failure text a child carries when its parent's coordinator died and the
// sweep canceled it, so the SQL stores stamp the same reason the in-memory one does.
func OrphanError() string { return orphanError }

// abandonedParentError is stamped on a split or pipeline parent left pending with no coordinator.
const abandonedParentError = "interrupted: no coordinator ever started this run"

// AbandonedParentError returns the failure text an abandoned parent carries, so every store stamps
// the same reason.
func AbandonedParentError() string { return abandonedParentError }

// AbandonedParent reports whether r is a split or pipeline parent that nothing will ever finish,
// given the cutoff a sweep is using. It is the single statement of a rule the SQL stores each
// express as a WHERE clause, so the four implementations can be read against one definition.
//
// A parent is abandoned when it is pending, unclaimed, and older than the cutoff. Each condition
// carries weight:
//
//   - Claim excludes every run with a Kind, so no worker will ever pick a parent up. A parent that
//     is not being coordinated in some process's memory is not waiting for anything.
//   - A live coordinator saves its parent running, with a lease, as its first act. So a parent past
//     the cutoff with no lease has no coordinator: the process that would have started one died
//     between saving the parent and starting it, or a child save failed and the submit returned
//     early leaving the children it had already written behind, or an approval released the parent
//     and then the process handling that approval went away.
//   - Held is not abandoned. A parent awaiting approval is resting, legitimately, for as long as it
//     takes a person to decide, and Approve starts its coordinator. Sweeping those would cancel
//     every gated split and workflow that outlived one sweep interval, which is why the status test
//     is pending and not merely non-terminal.
//
// Interrupting the parent is what makes the sweep settle its children, since orphan resolution keys
// off an interrupted parent. Without it the parent sits pending forever while its children stay
// claimable and run with nothing to roll them up.
func AbandonedParent(r *Run, cutoff time.Time) bool {
	if r.Kind != KindSplit && r.Kind != KindPipeline {
		return false
	}
	// Pending or running, both with no lease. Pending is a parent whose submit died before it could
	// start one. Running is a parent an approval released: an approved parent goes straight to
	// running so the sweep cannot catch it in the instant before its coordinator claims it, which
	// leaves running-and-unclaimed as the state that means the coordinator never arrived. Neither
	// the lease sweep, which only looks at leased runs, nor an earlier version of this rule, which
	// only looked at pending ones, covered that, so it was a parent nothing would ever finish.
	if r.Status != StatusPending && r.Status != StatusRunning {
		return false
	}
	return r.ClaimedBy == "" && r.CreatedAt.Before(cutoff)
}

// Finalization is everything an executor learns by running a run: how it ended and the facts that
// explain it. It is one value because a store writes it as one statement, so a run is never left
// terminal with the facts missing.
// Progress is what an executor learns about a run while it is still under way, as opposed to the
// facts that explain how it ended, which travel in a Finalization.
type Progress struct {
	// StartedAt is when execution began. It is applied only when the stored run has none, so a
	// repeated report cannot move a start time backward.
	StartedAt *time.Time
	// Warning is the executor's advisory note. Empty leaves the stored one alone.
	Warning string
	// Outputs are the set_stats values published so far. Nil leaves the stored ones alone.
	Outputs map[string]any
}

// Finalization holds the facts that explain how a run ended.
type Finalization struct {
	// Status is the terminal status the run reached.
	Status Status
	// ExitCode is the tool's exit code, nil when the run never produced one.
	ExitCode *int
	// Error is the failure detail, empty when the run ended without one.
	Error string
	// Image is the container image the run actually executed in, empty for a host run. It is part of
	// the terminal write because an executor only resolves the image while the run is under way, and
	// the outcome digest commits to it, so it has to land with the terminal status or the digest can
	// never be recomputed from the stored run.
	Image string
	// CommitSHA is the project commit the run executed, empty when it used no project. It is
	// resolved with the checkout while the run is under way, after the last whole-run save, so it
	// lands here or not at all, and the run dossier reports the provenance of what actually ran.
	CommitSHA string
	// PullCredentialID is the credential the run's image was pulled with, empty when none was used.
	// It is resolved with the project and is one of the grantable objects a run's authorization is
	// built from, so losing it silently narrows what the stored run is checked against.
	PullCredentialID string
	// Owner is the executor lease this write must still hold, and empty when the caller is not an
	// executor finalizing its own run.
	//
	// The terminal write fenced on status alone. That is enough on the relay path, where the HTTP
	// layer has already checked the per-claim capability, but not for a second in-process dispatcher
	// on a shared database: one whose pending-to-running save failed, then lost its heartbeats to a
	// partition, could come back after the janitor requeued the run and another worker claimed and
	// started it, and terminalize the run the second worker was still executing. Status matched,
	// because the second worker had made it running again.
	//
	// It is left empty deliberately by the two callers that are not executors: the sweep that ends a
	// run for overrunning its timeout, and the relay handler, which is gated on the claim secret
	// instead. Naming it here is what keeps that a stated choice rather than an omission.
	Owner string
	// Outputs are the values the run published with set_stats, which a later pipeline step reads as
	// its inputs. They are folded from the run's events as it finishes, so the terminal write is the
	// first and only chance to store them.
	Outputs map[string]any
	// Warning is the note a run carries about itself, such as having recorded no per-host result.
	// It is written while the run finishes, for the same reason.
	Warning string
	// EndedAt is when the run reached its terminal state.
	EndedAt time.Time
}

// Store persists runs, their captured log output, and their structured events.
// Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the run identified by r.ID. When r carries a non-empty
	// IdempotencyKey that a different run already holds, it makes no change and returns
	// ErrDuplicateKey, the race backstop that lets one of two concurrent submissions win the key.
	// A stored cancel request is sticky: replacing a run whose cancel flag is set keeps the flag
	// set, so saving a stale snapshot cannot erase a cancel another process just requested.
	Save(ctx context.Context, r *Run) error
	// Get returns the run with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Run, error)
	// ByIdempotencyKey returns the run that holds key, or ErrNotFound when no run does. An empty key
	// is never found, so a keyless submission is never deduped.
	ByIdempotencyKey(ctx context.Context, key string) (*Run, error)
	// List returns top-level runs, excluding shard runs, ordered by creation time, newest first.
	List(ctx context.Context) ([]*Run, error)
	// ListPage returns top-level runs matching filter, capped at limit and skipping offset, so the
	// runs view loads a page at a time. A limit of zero or less returns all of them.
	ListPage(ctx context.Context, filter ListFilter, limit, offset int) ([]*Run, error)
	// RunStatusCounts returns the number of top-level runs in each status. The runs view uses it
	// for the summary cards without loading every run.
	RunStatusCounts(ctx context.Context) (map[Status]int, error)
	// RunTimings returns the timing fields of the most recent top-level runs, newest first. It exists
	// so the metrics endpoint can build its histograms without reading whole runs: a run row carries
	// its extra vars, steps, labels, and notification targets, and decoding those for ten thousand
	// runs on every scrape costs far more than the seven values the histograms use.
	RunTimings(ctx context.Context, limit int) ([]RunTiming, error)
	// Shards returns the shard runs of a parent ordered by shard index.
	Shards(ctx context.Context, parentID string) ([]*Run, error)
	// Steps returns the pipeline step runs of a parent ordered by step index.
	Steps(ctx context.Context, parentID string) ([]*Run, error)
	// NonTerminal returns all runs, including shards, that are not in a terminal state.
	NonTerminal(ctx context.Context) ([]*Run, error)
	// Claim leases the oldest unclaimed pending executable run whose queue this owner serves and
	// returns it, or ErrNonePending when nothing is waiting. queues is the set of queue names the
	// caller serves; a run with an empty queue is on the default pool. Plain runs, shard
	// children, and pipeline step children are executable; split and pipeline parents are
	// coordination records and are not. The claim must be atomic across processes.
	Claim(ctx context.Context, owner string, queues []string) (*Run, error)
	// Heartbeat renews owner's lease on a run. It returns ErrNotFound when the run is gone or the
	// lease is no longer held by owner.
	Heartbeat(ctx context.Context, id, owner string) error
	// ReclaimStale requeues pending runs whose lease has gone unrenewed for longer than ttl and marks
	// stale running runs interrupted, returning how many rows changed. It sweeps up after dead
	// workers. It takes an age rather than an absolute cutoff so the store resolves it against the
	// same clock that stamped the lease: a caller computing the cutoff from its own clock would
	// interrupt healthy runs whenever the two clocks disagreed by more than ttl.
	ReclaimStale(ctx context.Context, ttl time.Duration) (int, error)
	// RequestCancel marks the run so whichever process holds it stops it, or ErrNotFound.
	RequestCancel(ctx context.Context, id string) error
	// CancelPending atomically cancels a run that is waiting unclaimed in pending or
	// pending_approval and reports whether it changed the run. It reports false for a missing,
	// claimed, executing, or terminal run; those are canceled cooperatively through RequestCancel
	// by whichever process holds them.
	CancelPending(ctx context.Context, id string) (bool, error)
	// TransitionStatusAndClaim atomically moves the run from the from status to the to status and
	// stamps owner's lease in the same operation, reporting whether it changed a row. It exists so a
	// run can never be observed in the to status without an owner: a parent released by an approval
	// goes straight to running, and a running parent with no lease is what the abandoned-parent
	// sweep settles, so two separate writes would let a janitor tick cancel a run an approver had
	// just released.
	TransitionStatusAndClaim(ctx context.Context, id string, from, to Status, owner string) (bool, error)
	// TransitionStatus atomically moves the run from the from status to the to status and reports
	// whether it changed a row. It changes nothing and returns false when the run is missing or is
	// not in the from status, so two callers racing to approve or reject the same run cannot both win.
	TransitionStatus(ctx context.Context, id string, from, to Status) (bool, error)
	// StampApprovedSpec records the spec digest an approver decided on, in a narrow write that
	// touches nothing else, so it cannot clobber a concurrent claim or cancel the way a full Save
	// from a stale snapshot would.
	StampApprovedSpec(ctx context.Context, id, digest string) error
	// FinalizeRunning atomically moves a running run to its terminal status and records the fields
	// that explain how it ended in the same write, reporting whether it changed a row. It changes
	// nothing and returns false when the run is missing or is no longer running, so an executor
	// cannot overwrite a terminal state another actor already recorded.
	//
	// The transition and the facts belong in one statement. Moving the status first and writing the
	// exit code, failure text, and end time after left a run terminal with none of them whenever the
	// second write failed, and a terminal run is swept by nothing: the janitor reclaims pending and
	// running runs only. The caller must treat a false or an error as "the run did not finish here"
	// and leave it for the sweep.
	FinalizeRunning(ctx context.Context, id string, fin Finalization) (bool, error)
	// ApplyRunningProgress records what an executor learned while a run is under way, in one write
	// fenced on the run still being running and still held by owner. It changes nothing and returns
	// false otherwise.
	//
	// The relay's progress handler used to re-read the row, check it was not terminal, and then save
	// the whole row back. Between that read and that write the janitor could settle the run, and the
	// save then restored the status, the lease, and the cleared claim secret from the snapshot: the
	// run was resurrected, the worker kept executing under a lease the control node had already
	// declared dead, and its later terminal report put a second, contradictory outcome on the audit
	// chain beside the interrupted one the sweep had already committed. A fenced write cannot do
	// that, for the same reason FinalizeRunning cannot.
	ApplyRunningProgress(ctx context.Context, id, owner string, p Progress) (bool, error)
	// Workers lists executors by the leases they hold, most recently seen first. Only leases
	// stamped within WorkerWindow count, so the listing stays bounded as run history grows.
	Workers(ctx context.Context) ([]WorkerInfo, error)
	// SaveHostSummary replaces the stored per host summaries for a run.
	SaveHostSummary(ctx context.Context, runID string, summaries []HostSummary) error
	// FleetHealth ranks hosts by failures over their most recent window runs, worst first.
	FleetHealth(ctx context.Context, window int) ([]HostHealth, error)
	// DriftStatus reports each host's most recent drift check, the latest dry run to touch it, worst
	// drift first. A host with no dry run in its history is omitted, having no drift signal.
	DriftStatus(ctx context.Context) ([]HostDrift, error)
	// HostCosts returns each host's average recorded duration in seconds over its most recent
	// window runs, for balancing splits by past cost.
	HostCosts(ctx context.Context, window int) (map[string]float64, error)
	// HostHistory returns a host's most recent per run summaries, newest first, with run ids.
	HostHistory(ctx context.Context, host string, limit int) ([]HostSummary, error)
	// RunHostSummaries returns one run's stored per host summaries, ordered by host.
	RunHostSummaries(ctx context.Context, runID string) ([]HostSummary, error)
	// SaveHostFacts records the system facts a run gathered, replacing what is held for each host.
	SaveHostFacts(ctx context.Context, runID string, facts []HostFacts) error
	// HostFactsFor returns a host's most recently gathered facts, or ErrNotFound when a host has
	// never been gathered.
	HostFactsFor(ctx context.Context, host string) (*HostFacts, error)
	// SaveTaskSummary replaces the stored per task summaries for a run.
	SaveTaskSummary(ctx context.Context, runID string, summaries []TaskSummary) error
	// RunTaskSummaries returns one run's stored per task summaries, ordered by task.
	RunTaskSummaries(ctx context.Context, runID string) ([]TaskSummary, error)
	// TaskTrends aggregates each task's durations over its most recent window runs.
	TaskTrends(ctx context.Context, window int) ([]TaskTrend, error)
	// AppendLog appends raw output bytes to the run's log. Returns ErrNotFound if the run is absent.
	AppendLog(ctx context.Context, id string, p []byte) error
	// Log returns a copy of the run's captured output, or ErrNotFound.
	Log(ctx context.Context, id string) ([]byte, error)
	// LogAfter returns the run's log chunks whose store sequence is greater than afterSeq, in
	// order, capped at limit chunks. A limit of zero or less returns every matching chunk. Each
	// chunk carries its Seq, so a caller streams new output by passing the last Seq it saw back
	// as afterSeq. Seq values are opaque and monotonic within a run. Returns ErrNotFound if the
	// run is absent.
	LogAfter(ctx context.Context, id string, afterSeq int64, limit int) ([]LogChunk, error)
	// LastLogSeq returns the store sequence of the run's most recent log chunk, or zero when the
	// run has no log. A live stream starts from it to send only what lands next without reading
	// the output already stored. Returns ErrNotFound if the run is absent.
	LastLogSeq(ctx context.Context, id string) (int64, error)
	// AppendEvents appends structured events to the run. Returns ErrNotFound if the run is absent.
	AppendEvents(ctx context.Context, id string, events []event.Event) error
	// Events returns a copy of the run's structured events, or ErrNotFound.
	Events(ctx context.Context, id string) ([]event.Event, error)
	// EventsAfter returns the run's events whose store sequence is greater than afterSeq, in
	// order, capped at limit. A limit of zero or less returns every matching event. Each event
	// carries its Seq, so a caller pages or streams by passing the last Seq it saw back as
	// afterSeq. Returns ErrNotFound if the run is absent.
	EventsAfter(ctx context.Context, id string, afterSeq int64, limit int) ([]event.Event, error)
	// LastEventSeq returns the store sequence of the run's most recent event, or zero when the
	// run has no events. A live stream starts from it to send only what lands next without
	// reading the existing log. Returns ErrNotFound if the run is absent.
	LastEventSeq(ctx context.Context, id string) (int64, error)
	// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping
	// the run records and their summaries. It returns how many runs were trimmed, counting only
	// runs that actually held events or logs to remove. A terminal run older than cutoff that had
	// nothing to trim is not counted, so the number reports runs whose data was removed, not every
	// eligible run.
	PurgeEventsBefore(ctx context.Context, cutoff time.Time) (int, error)
	// PurgeRunsBefore deletes terminal runs created before cutoff along with their events and logs,
	// keeping the per host and per task summaries that power the cross-run views. It returns how
	// many runs were deleted. Non-terminal runs are never purged.
	PurgeRunsBefore(ctx context.Context, cutoff time.Time) (int, error)
	// TrimSummaries keeps the newest keep per host summaries for each host and the newest keep per
	// task summaries for each task, deleting the rest, and returns how many rows it deleted. Nothing
	// else ever removes a summary: they outlive the runs they came from on purpose, so a host's
	// outcome history survives run retention. That makes this the only bound on those two tables,
	// which otherwise grow by one row per host per run forever. A keep below one is treated as one,
	// so no caller can empty the tables. Holding keep at or above MinRetainSummaries is the
	// configuration's job, not the store's.
	TrimSummaries(ctx context.Context, keep int) (int, error)
}

// SummaryAppender adds a batch of per-host or per-task summaries to a run's stored set, upserting by
// key and leaving the run's other rows untouched. Where SaveHostSummary replaces a run's entire set,
// Append only writes the rows it is given. A store that persists summaries implements it so a relay
// report split across many batches writes each batch once, rather than the relay server reading and
// rewriting the whole accumulated set on every continuation, which grows the work quadratically with
// the number of hosts. It is a focused capability, kept off Store so the transport stores that carry a
// report to the control node are not made to implement a persistence detail they never serve.
type SummaryAppender interface {
	// AppendHostSummary upserts the given per-host summaries into the run's set, keyed by host. It
	// fences a terminal run and is a no-op for an empty batch, matching SaveHostSummary.
	AppendHostSummary(ctx context.Context, runID string, summaries []HostSummary) error
	// AppendTaskSummary upserts the given per-task summaries into the run's set, keyed by task, with
	// the same fencing and empty-batch behavior.
	AppendTaskSummary(ctx context.Context, runID string, summaries []TaskSummary) error
}

// WorkerWindow bounds how far back Workers looks for leases. Terminal runs keep their last lease
// stamp, so without a bound the listing would aggregate every run ever recorded and report
// workers dead for months.
const WorkerWindow = 48 * time.Hour

// MaxLogBytes is the most captured output one run may accumulate.
//
// Nothing bounded it. Output is appended in chunks and the request that carries one is capped, but
// the total was not, so a run that prints without stopping grew the database until the disk did not
// take another byte. That is reachable by accident, from a playbook in a loop, and deliberately by
// anyone who can start a run or hold a worker token. The audit chain lives in the same database, so
// filling it takes the evidence down with the product.
//
// The bound is generous because a real run's output is evidence and truncating it costs something.
// A run that reaches this has stopped saying anything a reader will use.
const MaxLogBytes = 64 << 20

// LogTruncatedWarning is set on a run whose output stopped being captured at MaxLogBytes, so a
// reader is told the log is incomplete rather than left to think the run went quiet.
const LogTruncatedWarning = "output passed the capture limit and was truncated; the run continued"

// Summary window bounds. The window on the fleet and task trend views and the limit on host
// history are caller supplied, and every row a window admits becomes an element of the answer, so
// without a cap one request asks the store to rank, concatenate and serialize every summary ever
// recorded. MinRetainSummaries ties the retention trim to those caps: keeping at least as many
// rows as the largest window any caller may ask for makes the trim invisible through the API.
const (
	// MaxSummaryWindow is the largest window FleetHealth and TaskTrends accept from a caller. Both
	// return window entries for every host or task, so their cost is the window times the fleet's
	// cardinality, which is why it is the tighter of the two caps.
	MaxSummaryWindow = 100
	// MaxHostHistory is the largest limit HostHistory accepts from a caller. It reads one host's
	// rows straight off the ordered index, so it can afford a deeper page than the fleet views.
	MaxHostHistory = 500
	// MinRetainSummaries is the fewest summaries per host and per task a configured trim may leave
	// behind. It equals the largest caller-visible window, so trimmed history is history no view
	// could have reached. The retention sweeper raises a smaller setting to it; the stores trim to
	// whatever count they are given, so the floor is stated once, where the count is chosen.
	MinRetainSummaries = MaxHostHistory
)

// RunTiming is the slice of a run the metrics histograms need: when it was asked for, when it
// started, when it ended, and how it is grouped. It is deliberately small, because it is read in
// bulk on a schedule.
type RunTiming struct {
	// ID is the run's identifier, which a caller folding timings into a running total uses to tell
	// two runs sharing an end instant apart.
	ID string
	// Status is the run's current status.
	Status Status
	// Kind distinguishes a coordinator parent from an executable run, which is empty.
	Kind string
	// Queue is the queue the run was submitted to.
	Queue string
	// ClaimedBy is the executor holding the run, empty when none does.
	ClaimedBy string
	// CreatedAt is when the run was submitted.
	CreatedAt time.Time
	// StartedAt is when execution began, nil until it does.
	StartedAt *time.Time
	// EndedAt is when the run reached a terminal state, nil until it does.
	EndedAt *time.Time
}

// LogChunk is one stored piece of a run's log. Seq orders chunks within the run and serves as an
// opaque cursor for LogAfter.
type LogChunk struct {
	// Seq is the chunk's store sequence, monotonic within the run.
	Seq int64
	// Data is the chunk's raw bytes.
	Data []byte
}

// ListFilter narrows and orders a runs listing. Zero values apply no constraint, so the empty
// filter lists everything newest first.
type ListFilter struct {
	// Query is a free-text term matched case-insensitively across the fields the runs view shows.
	Query string
	// Status keeps only runs with exactly this status when set.
	Status string
	// Tool keeps only runs of this normalized tool when set. Ansible matches runs stored with an
	// empty tool, its historical form.
	Tool string
	// OldestFirst flips the newest-first default ordering.
	OldestFirst bool
	// After keeps only runs created at or after this time when set.
	After time.Time
	// Before keeps only runs created strictly before this time when set.
	Before time.Time
	// Source keeps only runs fired by this source when set: api, template, schedule, rerun,
	// reconcile, or propose.
	Source string
	// Actor keeps only runs fired by this authenticated user when set.
	Actor string
	// SourceID keeps only runs fired by that specific template, schedule, or origin run.
	SourceID string
	// Host keeps only runs that touched this host, resolved through the stored host summaries.
	Host string
	// LabelKey with LabelValue keeps only runs carrying that label pair.
	LabelKey string
	// LabelValue is the value LabelKey must hold.
	LabelValue string
	// ClaimedBy keeps only runs executed by this worker when set, so a worker's row can open the
	// work it actually did instead of being a dead end.
	ClaimedBy string
	// HeldBy keeps only runs held by the approval rule with this label when set. The field is a
	// historical record, so a caller wanting only the currently held ones pairs it with Status.
	HeldBy string
}
