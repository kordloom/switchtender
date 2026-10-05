package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/safedial"
	"github.com/kordloom/switchtender/internal/secretsource"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/util"
)

// Source is the run source a review plan carries, so a review plan is never mistaken for a run a
// push trigger fired and a restarted server can find the plans it was still reporting on.
const Source = "review"

// LabelPullRequest is the run label holding the pull request number a review plan reports to.
const LabelPullRequest = run.LabelPullRequest

// Reporting defaults.
const (
	// DefaultInterval is how often the reporter looks for reports to make.
	DefaultInterval = 2 * time.Second
	// DefaultMaxWait is how long a report keeps retrying a forge that fails it before it gives up.
	// An outage is retried through, and a token that was revoked stops being tried after this.
	DefaultMaxWait = 24 * time.Hour
	// DefaultOutcomeGrace is how long a finished plan's final report waits for the run's outcome
	// entry before it goes out without one. The outcome is committed the moment a run ends, so only
	// a chain that failed to record it makes the wait run out.
	DefaultOutcomeGrace = 2 * time.Minute
	// requestTimeout bounds one forge request.
	requestTimeout = 30 * time.Second
	// postTimeout bounds one whole delivery: reading the plan, the forge reads, the chain entry,
	// the comment, and the status.
	postTimeout = 5 * time.Minute
	// claimLease is how long a claim holds a report against every other process. It outlasts
	// postTimeout, so a claim lapses only once the process holding it has stopped trying, and a
	// second process never posts beside a slow first one.
	claimLease = postTimeout + time.Minute
	// maxBackoff caps the wait between attempts against a failing forge.
	maxBackoff = 5 * time.Minute
	// heldRecheck caps how long an unchanged held plan waits between reads. It waits for a
	// person, often for hours, and reading it every interval that long costs a query a tick for
	// nothing.
	heldRecheck = time.Minute
	// adoptEvery is how often the reporter looks for review plans no record covers.
	adoptEvery = time.Minute
	// adoptWindow is how far back a starting reporter looks for plans no record covers.
	adoptWindow = time.Hour
	// adoptLimit bounds how many plans one adoption pass reads.
	adoptLimit = 500
	// pendingLimit bounds how many records one sweep reads.
	pendingLimit = 1000
	// deliveries bounds how many reports one process delivers at once.
	deliveries = 4
	// waitPoll is how often Wait looks for the reporter to have nothing left to report.
	waitPoll = 10 * time.Millisecond
	// maxReason bounds a failure reason shown in a comment.
	maxReason = 300
	// maxLastError bounds the failure a record keeps and the run shows.
	maxLastError = 500
)

// noOutcomeNote is the line a final report carries when the run's outcome never reached the chain.
const noOutcomeNote = "_This plan finished, and its outcome did not reach the audit chain, so no " +
	"receipt can be issued for it. The result above is read from the run itself._"

// Redactor masks every secret this server can attribute to a run out of text that is about to leave
// it. The dispatcher satisfies it.
type Redactor interface {
	// RedactRunText returns text with r's secrets masked.
	RedactRunText(ctx context.Context, r *run.Run, text string) string
}

// Previewer decides what the rules in force would do with a run that has not been submitted. The
// dispatcher satisfies it.
type Previewer interface {
	// PreviewApply returns the decision apply would get, given the destroy count its plan reported
	// and whether that count was read at all.
	PreviewApply(ctx context.Context, apply *run.Run, destroys int, read bool) (dispatch.ApplyPreview, error)
}

// Config wires a Reporter. Runs, Templates, Credentials, and Sealer are required.
type Config struct {
	// Runs reads the plan runs, their logs, and their host results, and is the clock claims and
	// retries are measured with.
	Runs run.Store
	// Templates names the template a review reports on and builds the apply it previews.
	Templates template.Store
	// Triggers resolves the trigger each report posts through. Nil takes the store Resume is given.
	Triggers trigger.Store
	// Store holds the report records. Nil keeps them in this process's memory, which serves one
	// process and does not survive a restart.
	Store Store
	// Previewer decides the apply preview against the rules in force. Nil leaves the preview out.
	Previewer Previewer
	// Credentials holds the review's token credential.
	Credentials credential.Store
	// Sealer opens the token credential.
	Sealer *credential.Sealer
	// Audits records each report before it is sent, and holds the run outcomes a final report waits
	// for. Nil keeps no trail and waits for nothing.
	Audits audit.Store
	// Redactor masks plan output before it leaves the server. Nil withholds the output entirely,
	// since unmasked output must not be posted.
	Redactor Redactor
	// HTTPClient reaches the forge. Nil uses a client that refuses this server itself and cloud
	// metadata addresses and does not follow redirects.
	HTTPClient *http.Client
	// PublicURL is this server's externally reachable address, used to link the run. Empty posts
	// run ids without links.
	PublicURL string
	// Interval is how often the reporter sweeps for reports to make, and the first wait after a
	// failed attempt. Zero uses DefaultInterval.
	Interval time.Duration
	// MaxWait is how long a report keeps retrying a failing forge before it gives up. Zero uses
	// DefaultMaxWait.
	MaxWait time.Duration
	// OutcomeGrace is how long a final report waits for the run's outcome entry. Zero uses
	// DefaultOutcomeGrace.
	OutcomeGrace time.Duration
	// Done stops the reporter when closed, for server shutdown. Nil never stops it.
	Done <-chan struct{}
	// Log records reporting failures.
	Log *zap.Logger
}

// Reporter reports review plans and refusals to their pull requests.
//
// Every report is a record in the store, made when the webhook is handled, and every process
// sharing the store sweeps the records on an interval. A record that owes a report is claimed the
// way the scheduler claims a due schedule, by compare-and-set on the version read, so of every
// process looking exactly one reports it, and a pull request's comment is written by one process at
// a time. What landed is written back to the record, so a restart resumes from what the pull
// request was last told, a plan that finished while no server was running is reported when one
// starts, and a forge that fails is retried with backoff and shown on the run.
type Reporter struct {
	// cfg is the wiring.
	cfg Config
	// owner names this process on the claims it takes.
	owner string
	// kick wakes the loop for a record just made.
	kick chan struct{}
	// startOnce starts the loop once.
	startOnce sync.Once
	// started reports that the loop was started.
	started atomic.Bool
	// loopDone is closed when the loop has stopped.
	loopDone chan struct{}
	// wg tracks the loop and every delivery.
	wg sync.WaitGroup
	// slots bounds the deliveries in flight.
	slots chan struct{}
	// mu guards everything below.
	mu sync.Mutex
	// triggers resolves the trigger a record posts through.
	triggers trigger.Store
	// busy holds the records this process is delivering, so a sweep does not start a second.
	busy map[string]bool
	// idle holds when each unchanged held plan is next read.
	idle map[string]*recheck
	// scans holds, per run, the last chain sequence an outcome search read, so the wait for an
	// outcome reads each new entry once.
	scans map[string]int64
	// ended holds when this process first saw each finished run, read on the store's clock.
	ended map[string]time.Time
	// adoptedAt is when the last adoption pass ran, on this process's clock.
	adoptedAt time.Time
	// prunedAt is when comment records were last pruned, on this process's clock.
	prunedAt time.Time
}

// recheck is when an unchanged held plan is next read, and the gap that grows while it waits.
type recheck struct {
	// next is the earliest time, on this process's clock, the plan is read again.
	next time.Time
	// gap is the current wait between reads.
	gap time.Duration
}

// NewReporter returns a Reporter. It panics when a required dependency is missing, which is a
// wiring mistake caught at startup.
func NewReporter(cfg Config) *Reporter {
	if cfg.Runs == nil || cfg.Templates == nil || cfg.Credentials == nil || cfg.Sealer == nil {
		panic("review: Runs, Templates, Credentials, and Sealer are required")
	}
	if cfg.Store == nil {
		cfg.Store = NewMemStore()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = safedial.OffHostClient(requestTimeout)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = DefaultMaxWait
	}
	if cfg.OutcomeGrace <= 0 {
		cfg.OutcomeGrace = DefaultOutcomeGrace
	}
	if cfg.Log == nil {
		cfg.Log = zap.NewNop()
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	return &Reporter{
		cfg: cfg, owner: idgen.New("rep_", 6), kick: make(chan struct{}, 1),
		loopDone: make(chan struct{}), slots: make(chan struct{}, deliveries),
		triggers: cfg.Triggers, busy: map[string]bool{}, idle: map[string]*recheck{},
		scans: map[string]int64{},
		ended: map[string]time.Time{},
	}
}

// Watch reports the plan runID to pull request number, each change of phase once, until its final
// result has been reported. It records the plan and returns at once. The reports go out from the
// background, through the claim every process sharing the store competes for, so each is made once
// whichever process received the webhook.
func (rp *Reporter) Watch(tg *trigger.Trigger, number int, runID string) {
	if tg == nil || tg.Review == nil || runID == "" {
		return
	}
	rp.track(func(now time.Time) *Record { return PlanRecord(runID, tg, number, now) })
}

// Refuse reports that no plan was run for a pull request, with reason, as a comment and a commit
// status, in the background.
func (rp *Reporter) Refuse(tg *trigger.Trigger, ev *Event, reason, receipt string) {
	if tg == nil || tg.Review == nil || ev == nil {
		return
	}
	rp.track(func(now time.Time) *Record {
		return RefusalRecord(KindRefusal, tg, ev.Number, ev.HeadSHA, reason, receipt, now)
	})
}

// RefuseFork reports that a pull request from a fork was not planned, in the background, as a
// commit status alone: ForkStatus, linked to ForkDocsURL. It never writes a comment, so pull
// requests opened from forks cannot make the operator's token post anything on the pull request.
func (rp *Reporter) RefuseFork(tg *trigger.Trigger, ev *Event, receipt string) {
	if tg == nil || tg.Review == nil || ev == nil {
		return
	}
	rp.track(func(now time.Time) *Record {
		return RefusalRecord(KindFork, tg, ev.Number, ev.HeadSHA, "", receipt, now)
	})
}

// SkipNoHosts reports that a pull request's plan was skipped because the template's inventory
// matched no hosts, in the background, as a commit status alone: NoHostsStatus, linked to
// NoHostsDocsURL. Nothing went wrong with the pull request, so it writes no comment.
func (rp *Reporter) SkipNoHosts(tg *trigger.Trigger, ev *Event, receipt string) {
	if tg == nil || tg.Review == nil || ev == nil {
		return
	}
	rp.track(func(now time.Time) *Record {
		return RefusalRecord(KindNoHosts, tg, ev.Number, ev.HeadSHA, "", receipt, now)
	})
}

// track saves the record build makes, unless one with its id exists, and wakes the loop. A plan
// whose record cannot be saved is logged and made again by the next adoption pass, which finds the
// plan without one.
func (rp *Reporter) track(build func(time.Time) *Record) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	rec := build(rp.clock(ctx))
	if _, err := rp.cfg.Store.Create(ctx, rec); err != nil {
		rp.cfg.Log.Error("review: record a report: "+err.Error(), zap.String("record", rec.ID))
	}
	rp.start()
	rp.wake()
}

// Resume starts reporting on a server that has just started. Every review plan still in flight,
// whatever its age, and every plan of the last hour that no record covers gets a record, and the
// loop then reports whatever is owed, including the final result of a plan that finished while no
// server was running. triggers resolves each report's trigger when the configuration named none.
func (rp *Reporter) Resume(ctx context.Context, triggers trigger.Store) {
	if triggers != nil {
		rp.mu.Lock()
		if rp.triggers == nil {
			rp.triggers = triggers
		}
		rp.mu.Unlock()
	}
	rp.adopt(ctx, rp.clock(ctx), true)
	rp.start()
	rp.wake()
}

// Wait blocks until the reporter has nothing left to report and no report in flight, or, once Done
// has closed, until the loop has stopped and its last deliveries have finished, so nothing it
// started outlives a shutdown. A reporter that never started returns at once.
func (rp *Reporter) Wait() {
	if !rp.started.Load() {
		return
	}
	for rp.stopping() || !rp.drained() {
		select {
		case <-rp.loopDone:
			rp.wg.Wait()
			return
		case <-time.After(waitPoll):
		}
	}
}

// stopping reports whether Done has closed.
func (rp *Reporter) stopping() bool {
	if rp.cfg.Done == nil {
		return false
	}
	select {
	case <-rp.cfg.Done:
		return true
	default:
		return false
	}
}

// drained reports whether no record is pending in the store and no delivery is in flight here.
func (rp *Reporter) drained() bool {
	rp.mu.Lock()
	busy := len(rp.busy)
	rp.mu.Unlock()
	if busy > 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	pending, err := rp.cfg.Store.Pending(ctx, 1)
	return err == nil && len(pending) == 0
}

// start starts the loop once.
func (rp *Reporter) start() {
	rp.startOnce.Do(func() {
		rp.started.Store(true)
		rp.wg.Add(1)
		go rp.loop()
	})
}

// wake asks the loop to sweep now rather than at its next tick.
func (rp *Reporter) wake() {
	select {
	case rp.kick <- struct{}{}:
	default:
	}
}

// loop sweeps the store every interval, and at once whenever a report is recorded, until Done
// closes. Closing Done cancels the deliveries in flight, which release their claims for the next
// process to take.
func (rp *Reporter) loop() {
	defer rp.wg.Done()
	defer close(rp.loopDone)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if rp.cfg.Done != nil {
		go func() {
			select {
			case <-rp.cfg.Done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	ticker := time.NewTicker(rp.cfg.Interval)
	defer ticker.Stop()
	for {
		rp.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-rp.kick:
		}
	}
}

// sweep reads the pending records and hands each one that may owe a report to a delivery, as many
// at once as there are slots. A record another process holds, one backing off after a failure, an
// unchanged held plan between its reads, and one already in flight here are passed over.
func (rp *Reporter) sweep(ctx context.Context) {
	now := rp.clock(ctx)
	if rp.adoptDue() {
		rp.adopt(ctx, now, false)
	}
	recs, err := rp.cfg.Store.Pending(ctx, pendingLimit)
	if err != nil {
		if ctx.Err() == nil {
			rp.cfg.Log.Warn("review: list pending reports: " + err.Error())
		}
		return
	}
	rp.forget(recs)
	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		if rec.claimedAt(now) || rec.RetryAt.After(now) || !rp.recheckDue(rec.ID) {
			continue
		}
		if !rp.reserve(rec.ID) {
			continue
		}
		select {
		case rp.slots <- struct{}{}:
		default:
			// Every slot is in use. What is left waits for the next sweep.
			rp.unreserve(rec.ID)
			return
		}
		rp.wg.Add(1)
		go func(rec *Record) {
			defer rp.wg.Done()
			defer func() { <-rp.slots }()
			defer rp.unreserve(rec.ID)
			rp.process(ctx, rec, now)
		}(rec)
	}
}

// reserve marks id as in flight here and reports whether it was not already.
func (rp *Reporter) reserve(id string) bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.busy[id] {
		return false
	}
	rp.busy[id] = true
	return true
}

// unreserve clears id's in-flight mark.
func (rp *Reporter) unreserve(id string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	delete(rp.busy, id)
}

// forget drops what this process remembers about records no longer pending.
func (rp *Reporter) forget(pending []*Record) {
	keep := make(map[string]bool, len(pending))
	for _, r := range pending {
		keep[r.ID] = true
		keep[r.RunID] = true
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	for id := range rp.idle {
		if !keep[id] {
			delete(rp.idle, id)
		}
	}
	for id := range rp.scans {
		if !keep[id] {
			delete(rp.scans, id)
		}
	}
	for id := range rp.ended {
		if !keep[id] {
			delete(rp.ended, id)
		}
	}
}

// recheckDue reports whether record id is due to be read, which it always is unless it is a held
// plan that has not changed lately.
func (rp *Reporter) recheckDue(id string) bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	r, ok := rp.idle[id]
	return !ok || !time.Now().Before(r.next)
}

// heldUnchanged pushes the next read of an unchanged held plan out, doubling the gap from the
// interval up to heldRecheck, or thirty intervals when the interval is short.
func (rp *Reporter) heldUnchanged(id string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	r := rp.idle[id]
	if r == nil {
		r = &recheck{}
		rp.idle[id] = r
	}
	r.gap = min(max(r.gap*2, rp.cfg.Interval), heldRecheck, 30*rp.cfg.Interval)
	r.next = time.Now().Add(r.gap)
}

// changed resets the read schedule of record id, whose plan moved.
func (rp *Reporter) changed(id string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	delete(rp.idle, id)
}

// step is what a record owes next.
type step int

const (
	// stepNone means nothing is owed yet.
	stepNone step = iota
	// stepReport means a report is owed.
	stepReport
	// stepClose means the record is finished with nothing to post: its final result was already
	// reported, or its plan is gone.
	stepClose
)

// assessment is what assess decided about a record.
type assessment struct {
	// step is what the record owes.
	step step
	// run is the plan run as read, nil for a refusal or a plan that is gone.
	run *run.Run
	// noOutcome reports that a final report goes out without the run's outcome entry, because the
	// entry did not reach the chain within the grace.
	noOutcome bool
	// why says why a record closes with nothing posted, empty when its final result was reported.
	why string
}

// assess decides what rec owes. A refusal always owes its one report. A plan owes a report when
// its phase differs from the one last reported, except that a final result waits for the run's
// outcome entry, so the pull request is never told a plan finished before the chain records how.
func (rp *Reporter) assess(ctx context.Context, rec *Record, now time.Time) (assessment, error) {
	if rec.Kind != KindPlan {
		return assessment{step: stepReport}, nil
	}
	r, err := rp.cfg.Runs.Get(ctx, rec.RunID)
	if errors.Is(err, run.ErrNotFound) {
		return assessment{step: stepClose,
			why: "the plan run no longer exists, so nothing more can be reported"}, nil
	}
	if err != nil {
		return assessment{}, err
	}
	if reportKey(phaseOf(r), r.HeldByPolicy) == reportKey(rec.Phase, rec.HeldBy) {
		if r.Status.Terminal() {
			return assessment{step: stepClose, run: r}, nil
		}
		if r.Status == run.StatusPendingApproval {
			rp.heldUnchanged(rec.ID)
		}
		return assessment{step: stepNone, run: r}, nil
	}
	rp.changed(rec.ID)
	if !r.Status.Terminal() || rp.cfg.Audits == nil {
		return assessment{step: stepReport, run: r}, nil
	}
	recorded, err := rp.outcomeRecorded(ctx, r)
	if err != nil {
		return assessment{}, err
	}
	if recorded {
		return assessment{step: stepReport, run: r}, nil
	}
	if now.Sub(rp.finishedSeen(r.ID, now)) < rp.cfg.OutcomeGrace {
		return assessment{step: stepNone, run: r}, nil
	}
	return assessment{step: stepReport, run: r, noOutcome: true}, nil
}

// reportKey is what tells two reports of a plan apart: the phase, and for a held plan the rule
// holding it.
func reportKey(phase, heldBy string) string {
	if phase == PhaseHeld {
		return phase + "|" + heldBy
	}
	return phase
}

// finishedSeen returns when this process first saw the run id finished, read on the store's clock
// like now, recording now when this is the first time.
//
// The grace for a run's outcome is measured on that one clock. The run's own end time was stamped
// by whichever process executed it, and against the store's clock an executor running three minutes
// behind made a plan that ended a moment ago look three minutes old, so its final report went out at
// once, saying the outcome never reached the chain, and the outcome landed a moment later. A run is
// never seen finished before it finished, so the wait measured from first sight is never shorter
// than the grace.
func (rp *Reporter) finishedSeen(id string, now time.Time) time.Time {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	at, ok := rp.ended[id]
	if !ok {
		at = now
		rp.ended[id] = at
	}
	return at
}

// errOutcomeFound stops a chain scan at the outcome entry it was looking for.
var errOutcomeFound = errors.New("outcome found")

// outcomeRecorded reports whether r's outcome entry is on the audit chain. The scan starts at the
// entry that recorded r's webhook, since the outcome always follows it, and resumes where the last
// look stopped, so waiting on an outcome reads each new entry once.
func (rp *Reporter) outcomeRecorded(ctx context.Context, r *run.Run) (bool, error) {
	rp.mu.Lock()
	after, ok := rp.scans[r.ID]
	rp.mu.Unlock()
	if !ok {
		after = max(receiptSeq(r.AuditReceipt)-1, 0)
	}
	prefix := "/runs/" + r.ID + "/outcome/"
	last := after
	err := rp.cfg.Audits.ChainScan(ctx, after, func(e *audit.Entry) error {
		last = e.Seq
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, prefix) {
			return errOutcomeFound
		}
		return nil
	})
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if errors.Is(err, errOutcomeFound) {
		delete(rp.scans, r.ID)
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the audit chain for the plan's outcome: %w", err)
	}
	rp.scans[r.ID] = last
	return false, nil
}

// receiptSeq returns the sequence a seq:link receipt names, zero when it names none.
func receiptSeq(receipt string) int64 {
	seq, _, _ := strings.Cut(receipt, ":")
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// process decides whether rec owes a report and, when it does, claims it and delivers it. Of every
// process that read the same version only one wins the claim, and the rest leave the record alone.
func (rp *Reporter) process(ctx context.Context, rec *Record, now time.Time) {
	a, err := rp.assess(ctx, rec, now)
	if err != nil {
		if ctx.Err() == nil {
			rp.cfg.Log.Warn("review: read a report's plan: "+err.Error(),
				zap.String("record", rec.ID))
		}
		return
	}
	if a.step == stepNone {
		return
	}
	until := now.Add(claimLease)
	won, err := rp.cfg.Store.Claim(ctx, rec.ID, rec.Version, rp.owner, now, until)
	if err != nil {
		if ctx.Err() == nil {
			rp.cfg.Log.Warn("review: claim a report: "+err.Error(), zap.String("record", rec.ID))
		}
		return
	}
	if !won {
		return
	}
	rec.Version++
	rec.ClaimedBy, rec.ClaimedUntil = rp.owner, until
	rp.deliver(ctx, rec)
}

// errTriggerGone is returned when a report's trigger no longer exists or no longer reviews.
var errTriggerGone = errors.New("review trigger gone")

// deliver makes the report a claimed record owes and settles the record with how it went. The
// plan is read again under the claim, so what is posted is the newest state, and everything runs
// under postTimeout, which the claim's lease outlasts.
func (rp *Reporter) deliver(ctx context.Context, rec *Record) {
	dctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	a, err := rp.assess(dctx, rec, rp.clock(dctx))
	if err != nil {
		rp.failed(ctx, rec, err)
		return
	}
	switch a.step {
	case stepNone:
		rp.settle(rec)
		return
	case stepClose:
		rec.Done = true
		if a.why != "" {
			rec.LastError = a.why
		}
		rp.settle(rec)
		return
	}
	tg, err := rp.trigger(dctx, rec.TriggerID)
	if errors.Is(err, errTriggerGone) {
		rec.Done = true
		rec.LastError = "the review trigger was deleted or no longer reviews, so the report " +
			"could not be posted"
		rp.settle(rec)
		return
	}
	if err != nil {
		rp.failed(ctx, rec, err)
		return
	}
	rep := rp.reportFor(dctx, tg, rec, a)
	res, err := rp.post(dctx, tg, rec, rep)
	if res.recorded != "" {
		rec.RecordedSHA256 = res.recorded
	}
	switch {
	case errors.Is(err, errClaimLost):
		// Another process holds the record now and reports from what it reads. Settling here would
		// be refused for the same reason, and counting a failure would be wrong.
		rp.cfg.Log.Warn("review: a report's claim lapsed before it was written, so another process "+
			"holds it now", zap.String("record", rec.ID))
		return
	case errors.Is(err, errForgeMoved):
		rec.Done = true
		rec.LastError = util.Clip(err.Error(), maxLastError)
		rp.settle(rec)
		return
	case err != nil:
		rp.failed(ctx, rec, err)
		return
	}
	if a.noOutcome {
		rp.cfg.Log.Error("review: posted a final report although the run's outcome is not on the "+
			"audit chain", zap.String("run_id", rec.RunID))
	}
	rec.Phase, rec.HeldBy = rep.Phase, rep.HeldBy
	rec.StatusState, rec.CommentSHA256 = res.state, res.comment
	if res.context != "" {
		rec.StatusContext = res.context
	}
	rec.ReportedAt = rp.clock(dctx)
	rec.Attempts, rec.FailingSince, rec.RetryAt, rec.LastError = 0, time.Time{}, time.Time{}, ""
	rec.Done = rec.Kind != KindPlan || (a.run != nil && a.run.Status.Terminal())
	rp.settle(rec)
}

// failed records an attempt that did not land and schedules the next, backing off from the
// interval and doubling up to maxBackoff. A forge that keeps failing for longer than MaxWait is
// given up on, and the record says so, where the run shows it. A delivery cut short by shutdown is
// not counted: the record is released for the next process to take.
func (rp *Reporter) failed(ctx context.Context, rec *Record, err error) {
	if ctx.Err() != nil {
		rp.settle(rec)
		return
	}
	cctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	now := rp.clock(cctx)
	cancel()
	msg := util.Clip(err.Error(), maxLastError)
	if rec.FailingSince.IsZero() {
		rec.FailingSince = now
	}
	rec.Attempts++
	rec.LastError = msg
	rec.RetryAt = now.Add(rp.backoff(rec.Attempts))
	if now.Sub(rec.FailingSince) >= rp.cfg.MaxWait {
		rec.Done, rec.RetryAt = true, time.Time{}
		rec.LastError = util.Clip("gave up after "+rp.cfg.MaxWait.String()+" of failed attempts: "+
			msg, maxLastError)
		rp.cfg.Log.Error("review: gave up reporting to the pull request: "+msg,
			zap.String("record", rec.ID), zap.Int("attempts", rec.Attempts))
	} else {
		rp.cfg.Log.Warn("review: report to the pull request failed, retrying: "+msg,
			zap.String("record", rec.ID), zap.Int("attempts", rec.Attempts),
			zap.Time("retry_at", rec.RetryAt))
	}
	rp.settle(rec)
}

// backoff returns the wait before attempt number attempts+1: the interval, doubled for each
// attempt after the first, capped at maxBackoff.
func (rp *Reporter) backoff(attempts int) time.Duration {
	d := rp.cfg.Interval
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// settle writes rec back and releases its claim, on a context of its own so a delivery that
// shutdown canceled still records how it ended.
func (rp *Reporter) settle(rec *Record) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	ok, err := rp.cfg.Store.Settle(ctx, rec, rec.Version)
	switch {
	case err != nil:
		rp.cfg.Log.Error("review: settle a report: "+err.Error(), zap.String("record", rec.ID))
	case !ok:
		rp.cfg.Log.Warn("review: a report's claim lapsed before it settled, so another process "+
			"holds it now", zap.String("record", rec.ID))
	}
}

// clock reads the store's clock, the one claims and retries are measured with, or this process's
// clock when the store cannot answer.
func (rp *Reporter) clock(ctx context.Context) time.Time {
	if now, err := rp.cfg.Runs.Now(ctx); err == nil {
		return now
	}
	return time.Now()
}

// trigger returns the review trigger with id, or errTriggerGone when it is deleted or no longer
// reviews.
func (rp *Reporter) trigger(ctx context.Context, id string) (*trigger.Trigger, error) {
	rp.mu.Lock()
	store := rp.triggers
	rp.mu.Unlock()
	if store == nil {
		return nil, errors.New("this server has no trigger store to read the review from")
	}
	tg, err := store.Get(ctx, id)
	if errors.Is(err, trigger.ErrNotFound) {
		return nil, errTriggerGone
	}
	if err != nil {
		return nil, fmt.Errorf("read the review trigger: %w", err)
	}
	if tg.Review == nil {
		return nil, errTriggerGone
	}
	return tg, nil
}

// adoptDue reports whether an adoption pass is due.
func (rp *Reporter) adoptDue() bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.adoptedAt.IsZero() || time.Since(rp.adoptedAt) >= adoptEvery
}

// adopt makes a record for every review plan that has none, so a plan whose webhook reached a
// process that stopped before recording it is still reported. A starting server passes all, which
// takes in every plan still in flight, whatever its age, and the plans of the last adoptWindow. The
// periodic pass looks back three passes, which covers a replica that stopped between launching a
// plan and recording it.
func (rp *Reporter) adopt(ctx context.Context, now time.Time, all bool) {
	rp.mu.Lock()
	rp.adoptedAt = time.Now()
	prune := rp.adoptedAt.Sub(rp.prunedAt) >= pruneEvery
	if prune {
		rp.prunedAt = rp.adoptedAt
	}
	rp.mu.Unlock()
	if prune {
		if _, err := rp.cfg.Store.PruneComments(ctx, now.Add(-CommentRetention)); err != nil &&
			ctx.Err() == nil {
			rp.cfg.Log.Warn("review: prune comment records: " + err.Error())
		}
	}
	window := 3 * adoptEvery
	if all {
		window = adoptWindow
	}
	plans, err := rp.cfg.Runs.ListPage(ctx, run.ListFilter{Source: Source, After: now.Add(-window)},
		adoptLimit, 0)
	if err != nil && ctx.Err() == nil {
		rp.cfg.Log.Warn("review: list plans to adopt: " + err.Error())
	}
	if all {
		live, err := rp.cfg.Runs.NonTerminal(ctx)
		if err != nil && ctx.Err() == nil {
			rp.cfg.Log.Warn("review: list plans in flight to adopt: " + err.Error())
		}
		plans = append(plans, live...)
	}
	for _, r := range plans {
		if r.Source != Source || r.ParentID != nil || r.SourceID == "" {
			continue
		}
		number, err := strconv.Atoi(r.Labels[LabelPullRequest])
		if err != nil || number <= 0 {
			continue
		}
		if _, err := rp.cfg.Store.Get(ctx, r.ID); !errors.Is(err, ErrRecordNotFound) {
			continue
		}
		// The record reports where the trigger points now, the closest this pass can come to where
		// it pointed when the plan launched a few minutes ago. A trigger since deleted leaves a
		// record that closes at its first delivery and says why.
		tg, err := rp.trigger(ctx, r.SourceID)
		if errors.Is(err, errTriggerGone) {
			tg, err = &trigger.Trigger{ID: r.SourceID}, nil
		}
		if err != nil {
			rp.cfg.Log.Warn("review: adopt a plan: "+err.Error(), zap.String("run_id", r.ID))
			continue
		}
		created, err := rp.cfg.Store.Create(ctx, PlanRecord(r.ID, tg, number, now))
		if err != nil {
			rp.cfg.Log.Warn("review: adopt a plan: "+err.Error(), zap.String("run_id", r.ID))
			continue
		}
		if created {
			rp.cfg.Log.Info("review: adopted a plan no report record covered",
				zap.String("run_id", r.ID))
		}
	}
}

// phaseOf maps a run's status onto a review phase.
func phaseOf(r *run.Run) string {
	switch r.Status {
	case run.StatusPendingApproval:
		return PhaseHeld
	case run.StatusSucceeded:
		return PhaseSucceeded
	case run.StatusFailed, run.StatusInterrupted, run.StatusRejected:
		return PhaseFailed
	case run.StatusCanceled:
		return PhaseCanceled
	default:
		return PhaseRunning
	}
}

// templateName returns the template's name, or its id when it cannot be read.
func (rp *Reporter) templateName(ctx context.Context, id string) string {
	t, err := rp.cfg.Templates.Get(ctx, id)
	if err != nil || t.Name == "" {
		return id
	}
	return t.Name
}

// link returns path under the public address, or empty when there is none.
func (rp *Reporter) link(path string) string {
	if rp.cfg.PublicURL == "" {
		return ""
	}
	return rp.cfg.PublicURL + path
}

// redact masks text for r, or reports false when this reporter has no masker and the text must be
// withheld.
func (rp *Reporter) redact(ctx context.Context, r *run.Run, text string) (string, bool) {
	if rp.cfg.Redactor == nil {
		return "", false
	}
	return rp.cfg.Redactor.RedactRunText(ctx, r, text), true
}

// reportFor builds the report rec owes from what assess read.
func (rp *Reporter) reportFor(ctx context.Context, tg *trigger.Trigger, rec *Record, a assessment) Report {
	if rec.Kind == KindReply {
		return Report{TemplateID: tg.TemplateID, Phase: PhaseReplied, Reason: rec.Reason,
			At: rec.CreatedAt}
	}
	if rec.Kind != KindPlan {
		return Report{
			TemplateID: tg.TemplateID, TemplateName: rp.templateName(ctx, tg.TemplateID),
			Phase: PhaseRefused, Reason: rec.Reason, CommitSHA: rec.CommitSHA, Receipt: rec.Receipt,
			At: rec.CreatedAt, Fork: rec.Kind == KindFork, NoHosts: rec.Kind == KindNoHosts,
		}
	}
	rep := rp.planReport(ctx, tg, a.run)
	if a.noOutcome {
		rep.Note = noOutcomeNote
	}
	return rep
}

// planReport builds the report for plan run r. The template is the plan's own when the run names
// it, so a trigger pointed at another template while the plan is in flight cannot move its report
// onto that template's comment.
func (rp *Reporter) planReport(ctx context.Context, tg *trigger.Trigger, r *run.Run) Report {
	templateID := tg.TemplateID
	if r.TemplateID != "" {
		templateID = r.TemplateID
	}
	rep := Report{
		TemplateID: templateID, TemplateName: rp.templateName(ctx, templateID),
		Phase: phaseOf(r), HeldBy: r.HeldByPolicy, RunID: r.ID, Receipt: r.AuditReceipt,
		RunURL: rp.link("/ui/runs/" + r.ID), CommitSHA: r.PinnedCommit, Tool: r.Tool,
		ApprovalsURL: rp.link("/ui/runs?status=pending_approval"), At: r.CreatedAt,
	}
	if rep.Phase == PhaseSucceeded || rep.Phase == PhaseFailed {
		rp.fillResult(ctx, &rep, r)
	}
	if r.Status == run.StatusRejected {
		rep.Reason = "An approver rejected this plan in SwitchTender."
	}
	return rep
}

// fillResult adds a finished plan's summary, the apply preview, the failure reason, and the masked
// output excerpt to rep.
func (rp *Reporter) fillResult(ctx context.Context, rep *Report, r *run.Run) {
	raw, err := rp.cfg.Runs.Log(ctx, r.ID)
	if err != nil {
		rp.cfg.Log.Warn("review: read plan log: "+err.Error(), zap.String("run_id", r.ID))
	}
	out := string(raw)
	tool := run.NormalizeTool(r.Tool)
	destroys, read := 0, false
	if tool == run.ToolTerraform || tool == run.ToolOpenTofu {
		counts, ok := dispatch.PlanSummary(out)
		// A plan longer than the gate reads is a plan the gate would not have weighed, so it is
		// reported unread here for the same reason.
		read = ok && len(raw) <= dispatch.PlanReadCap
		rep.Plan, rep.PlanRead, destroys = &counts, read, counts.Destroy
	} else if hosts, err := rp.cfg.Runs.RunHostSummaries(ctx, r.ID); err == nil {
		rep.Hosts = hosts
	}
	if rep.Phase == PhaseSucceeded {
		rep.Preview = rp.preview(ctx, rep.TemplateID, destroys, read)
	}
	if rep.Phase == PhaseFailed && r.Error != "" {
		if reason, ok := rp.redact(ctx, r, r.Error); ok {
			if len(reason) > maxReason {
				reason = strings.ToValidUTF8(reason[:maxReason], "") + "..."
			}
			rep.Reason = code(reason)
		}
	}
	if out == "" {
		return
	}
	masked, ok := rp.redact(ctx, r, out)
	if !ok {
		rep.ExcerptNote = "_Plan output withheld: this server has no masker for it. Open the run " +
			"in SwitchTender to read it._"
		return
	}
	rep.Excerpt, rep.ExcerptNote = Excerpt(masked, r.Tool, rep.RunURL)
}

// preview returns the decision the template's ordinary run would get under today's rules, given the
// destroy count this plan reported, or nil when the template or the rules cannot be read.
func (rp *Reporter) preview(ctx context.Context, templateID string, destroys int,
	read bool) *dispatch.ApplyPreview {
	if rp.cfg.Previewer == nil {
		return nil
	}
	t, err := rp.cfg.Templates.Get(ctx, templateID)
	if err != nil {
		return nil
	}
	apply := &run.Run{ID: "run_apply_preview", Playbook: t.Playbook, Inventory: t.Inventory,
		OrgID: t.OrgID}
	run.ApplyOptions(apply, t.LaunchOptions())
	p, err := rp.cfg.Previewer.PreviewApply(ctx, apply, destroys, read)
	if err != nil {
		rp.cfg.Log.Warn("review: preview the apply: " + err.Error())
		return nil
	}
	return &p
}

// Why a report left the pull request's comment alone, as its chain entry records it.
const (
	// skippedSuperseded means the comment belongs to the plan of a newer push.
	skippedSuperseded = "superseded"
	// skippedFork means the report is a fork's refusal, which sets a commit status alone.
	skippedFork = "fork"
	// skippedNoHosts means the report is a plan skipped because its inventory matched no hosts,
	// which sets a commit status alone.
	skippedNoHosts = "no_hosts"
)

// reportRecord is the canonical content a report's chain entry commits to: which plan, which phase,
// which commit, and a digest of the exact comment body sent, so the trail proves what was said on
// the pull request without carrying the text.
type reportRecord struct {
	// Run is the plan run, empty for a refusal.
	Run string `json:"run,omitempty"`
	// Phase is the report's phase.
	Phase string `json:"phase"`
	// Commit is the commit the report describes.
	Commit string `json:"commit"`
	// Status is the commit status state set.
	Status string `json:"status"`
	// CommentSHA256 is the hex SHA-256 of the comment body, empty when no comment was written.
	CommentSHA256 string `json:"comment_sha256"`
	// CommentSkipped says why no comment was written: superseded when the comment belongs to the
	// plan of a newer push, fork for a fork's refusal. Omitted when the comment was written.
	CommentSkipped string `json:"comment_skipped,omitempty"`
}

// posted is what a delivered report left on the pull request.
type posted struct {
	// state is the commit status state set, empty when there was no commit to set it on.
	state string
	// comment is the SHA-256 of the comment body written, empty when the comment was left alone.
	comment string
	// recorded is the SHA-256 of the report content the chain holds an entry for, set as soon as
	// the entry is recorded, so a retry after a failed write does not record the same report twice.
	recorded string
	// context is the commit status context the status was set under, empty when none was set.
	context string
}

// commentAction is what a report does to the pull request's review comment.
type commentAction int

const (
	// commentNone leaves the comment alone.
	commentNone commentAction = iota
	// commentCreate posts a new comment.
	commentCreate
	// commentUpdate replaces the existing comment's body.
	commentUpdate
)

// commentFor decides what rep does to the pull request's review comment, given the commit the pull
// request proposes now, empty when the forge would not say, and the comment it already holds, nil
// when it holds none.
//
// The comment belongs to the newest push. A report about a commit the pull request has moved past
// never takes the comment over, even when its webhook arrived after the newer push's and its plan
// is the later run: it only keeps a comment that already describes it current. A report about the
// head never replaces the comment of a later plan of that same head. With no head to compare
// against, the plans' own times decide, the later plan keeping the comment.
func commentFor(rep Report, head string, existing *Comment) commentAction {
	if existing == nil {
		return commentCreate
	}
	m, ok := parseMarker(existing.Body)
	at := rep.At.UnixNano()
	if ok && m.Run == rep.RunID && m.At == at {
		return commentUpdate
	}
	switch {
	case head != "" && !strings.EqualFold(head, rep.CommitSHA):
		return commentNone
	case head != "" && ok && strings.EqualFold(m.Commit, head) && m.At > at:
		return commentNone
	case head == "" && ok && m.At > at:
		return commentNone
	default:
		return commentUpdate
	}
}

// post records rep on the chain, then writes its comment and commit status to the pull request.
//
// The forge is read first, the pull request's head and the comment it holds, to decide what the
// report writes, and nothing is written until the chain holds an entry committing to exactly that:
// a report that cannot be recorded is not sent, the order every outbound change the boundary makes
// follows. Content the chain already holds an entry for, the case of a retry after a write failed,
// is not recorded again. A fork's refusal, and a plan skipped because its inventory matched no hosts,
// read nothing and write the status alone.
//
// The report goes to the repository rec was made for, through the trigger's forge and token, and
// its status keeps the context the record's first status was set under. A record delivered under a
// claim is checked against the store before each write to the forge, so a process that stopped for
// longer than its claim, after deciding what to write and before writing it, writes nothing once it
// resumes: the process that took the lapsed claim has already reported.
func (rp *Reporter) post(ctx context.Context, tg *trigger.Trigger, rec *Record, rep Report) (posted, error) {
	cfg, err := destination(tg, rec)
	if err != nil {
		return posted{}, err
	}
	client, lease, err := rp.client(ctx, cfg)
	defer revoke(lease)
	if err != nil {
		return posted{}, err
	}
	if rec.Kind == KindReply {
		return rp.postReply(ctx, client, tg, rec)
	}
	body := Render(rep)
	statusContext := rec.StatusContext
	if statusContext == "" {
		statusContext = "switchtender/" + rep.TemplateName
	}
	status := StatusFor(rep, statusContext)
	action := commentNone
	var existing *Comment
	if !rep.Fork && !rep.NoHosts {
		head, err := client.Head(ctx, rec.PullRequest)
		if err != nil {
			return posted{}, fmt.Errorf("read the pull request's head: %w", err)
		}
		existing, err = client.FindComment(ctx, rec.PullRequest, MarkerPrefix(rep.TemplateID))
		if err != nil {
			return posted{}, fmt.Errorf("find review comment: %w", err)
		}
		action = commentFor(rep, head, existing)
	}
	content := reportRecord{Run: rep.RunID, Phase: rep.Phase, Commit: rep.CommitSHA,
		Status: status.State}
	switch {
	case action != commentNone:
		content.CommentSHA256 = digest([]byte(body))
	case rep.Fork:
		content.CommentSkipped = skippedFork
	case rep.NoHosts:
		content.CommentSkipped = skippedNoHosts
	default:
		content.CommentSkipped = skippedSuperseded
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return posted{}, err
	}
	res := posted{}
	if sum := digest(raw); sum != rec.RecordedSHA256 {
		if err := rp.record(ctx, tg, rec.PullRequest, raw); err != nil {
			return posted{}, err
		}
		res.recorded = sum
	}
	if action != commentNone {
		if err := rp.holding(ctx, rec); err != nil {
			return res, err
		}
	}
	switch action {
	case commentCreate:
		_, err = client.CreateComment(ctx, rec.PullRequest, body)
	case commentUpdate:
		err = client.UpdateComment(ctx, rec.PullRequest, existing.ID, body)
		if isRefused(err) {
			_, err = client.CreateComment(ctx, rec.PullRequest, body)
		}
	}
	if err != nil {
		return res, fmt.Errorf("write review comment: %w", err)
	}
	res.comment = content.CommentSHA256
	if rep.CommitSHA != "" {
		if err := rp.holding(ctx, rec); err != nil {
			return res, err
		}
		if err := client.SetStatus(ctx, rep.CommitSHA, status); err != nil {
			return res, fmt.Errorf("set commit status: %w", err)
		}
		res.state, res.context = status.State, statusContext
	}
	return res, nil
}

// errClaimLost is returned when a record's claim was lost before a write to the forge: its version
// moved, another process took it, or it lapsed by the store's clock.
var errClaimLost = errors.New("the report's claim lapsed")

// holding returns errClaimLost when rec carries a claim and the store no longer shows it held. A
// record that carries no claim has none to check.
func (rp *Reporter) holding(ctx context.Context, rec *Record) error {
	if rec.ClaimedBy == "" {
		return nil
	}
	stored, err := rp.cfg.Store.Get(ctx, rec.ID)
	if errors.Is(err, ErrRecordNotFound) {
		return errClaimLost
	}
	if err != nil {
		return fmt.Errorf("check the report's claim: %w", err)
	}
	if stored.Version != rec.Version || stored.ClaimedBy != rec.ClaimedBy ||
		!stored.ClaimedUntil.After(rp.clock(ctx)) {
		return errClaimLost
	}
	return nil
}

// errForgeMoved is returned when a report's trigger now points at another forge than the one its
// pull request is on.
var errForgeMoved = errors.New("the review trigger now reports to another forge")

// destination returns the review configuration rec's report is posted through: the trigger's, aimed
// at the repository rec was made for. A record an earlier release made names no repository and
// takes the trigger's now, which its settle then keeps. A trigger pointed at another forge cannot
// carry the report at all, because its token belongs to that forge and is never sent to another.
func destination(tg *trigger.Trigger, rec *Record) (*trigger.Review, error) {
	cfg := *tg.Review
	if rec.Repository == "" {
		rec.setDestination(&cfg)
		return &cfg, nil
	}
	if rec.Provider != cfg.Provider || rec.APIURL != cfg.BaseURL() {
		return nil, fmt.Errorf("%w: it reports to %s at %s, and this pull request is in %s on %s at "+
			"%s, so the report was not posted", errForgeMoved, cfg.Provider, cfg.BaseURL(),
			rec.Repository, rec.Provider, rec.APIURL)
	}
	cfg.Repository = rec.Repository
	return &cfg, nil
}

// digest returns the hex SHA-256 of b.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// record appends the chain entry for a report's content, fail-closed.
func (rp *Reporter) record(ctx context.Context, tg *trigger.Trigger, number int, content []byte) error {
	if rp.cfg.Audits == nil {
		return nil
	}
	entry := &audit.Entry{
		ID: audit.NewID(), Actor: "webhook:" + tg.ID, Method: http.MethodPost,
		Path: "/hooks/" + tg.ID + "/review/" + strconv.Itoa(number) + "/report",
	}
	var err error
	if entry.ContentDigest, entry.Nonce, err = audit.ContentDigestOf(content); err != nil {
		return fmt.Errorf("digest review report: %w", err)
	}
	if err := rp.cfg.Audits.Append(ctx, entry); err != nil {
		return fmt.Errorf("record review report: %w", err)
	}
	return nil
}

// client opens the review's token and returns a forge client holding it, with the token's lease for
// the caller to revoke once its requests are done. The plaintext lives only inside the client.
func (rp *Reporter) client(ctx context.Context, cfg *trigger.Review) (Client, *secretsource.Lease, error) {
	token, lease, err := rp.openToken(ctx, cfg.CredentialID)
	if err != nil {
		return nil, nil, err
	}
	c, err := NewClient(cfg, token, rp.cfg.HTTPClient)
	return c, lease, err
}

// openToken resolves the review's token credential through its source. The returned lease, nil for
// a static token, is revoked by the caller once the forge requests are done.
func (rp *Reporter) openToken(ctx context.Context, id string) (string, *secretsource.Lease, error) {
	c, err := rp.cfg.Credentials.Get(ctx, id)
	if err != nil {
		return "", nil, fmt.Errorf("%w: credential %s: %w", ErrNoToken, id, err)
	}
	plain, err := rp.cfg.Sealer.Open(c.Secret)
	if err != nil {
		return "", nil, fmt.Errorf("%w: credential %s: %w", ErrNoToken, id, err)
	}
	value, lease, err := secretsource.ResolveLeased(ctx, c.Source, plain)
	if err != nil {
		return "", nil, fmt.Errorf("%w: credential %s: %w", ErrNoToken, id, err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		revoke(lease)
		return "", nil, fmt.Errorf("%w: credential %s is empty", ErrNoToken, id)
	}
	return value, lease, nil
}

// revoke hands a dynamic token back on its own timeout. A nil lease is a static token.
func revoke(lease *secretsource.Lease) {
	if lease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	_ = lease.Revoke(ctx)
}

// CheckToken reports whether the credential id can serve as a review token: it exists and is a
// token credential. It is checked when a review trigger is written, so a trigger that could never
// report is refused there.
func CheckToken(ctx context.Context, creds credential.Store, id string) error {
	if creds == nil {
		return fmt.Errorf("%w: credentials are not enabled on this server", ErrNoToken)
	}
	c, err := creds.Get(ctx, id)
	if errors.Is(err, credential.ErrNotFound) {
		return fmt.Errorf("%w: credential %s not found", ErrNoToken, id)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoToken, err)
	}
	if c.Kind != credential.KindToken {
		return fmt.Errorf("%w: credential %s is kind %s, and a review posts with a token credential",
			ErrNoToken, id, c.Kind)
	}
	return nil
}
