package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// Outbox defaults.
const (
	// defaultPoll is how often an idle outbox looks for deliveries that came due, such as a retry
	// whose wait ran out or an event another process recorded.
	defaultPoll = 2 * time.Second
	// defaultLease is how long a claim on a delivery lasts. It is far longer than an attempt may
	// take, so a claim lapses only when the worker holding it is gone.
	defaultLease = time.Minute
	// defaultBatch bounds how many deliveries one claim takes.
	defaultBatch = 32
	// defaultPerTarget bounds how many attempts at one target run at once. A target that accepts a
	// connection and never answers holds each attempt it is given until the attempt times out, so
	// this is all of the outbox it can occupy, and every other target's deliveries go ahead beside
	// it.
	defaultPerTarget = 4
	// defaultWorkers bounds how many attempts run at once in all, a guard on goroutines and
	// connections rather than a share targets compete for: sixteen targets would have to hang at
	// once to fill it.
	defaultWorkers = 64
	// defaultAttemptTimeout bounds one attempt, an email included.
	defaultAttemptTimeout = 15 * time.Second
	// recordTimeout bounds recording one event, its retries included, since it runs on the path of
	// the run it describes.
	recordTimeout = 5 * time.Second
	// recordRetry is the first wait before recording an event again after the store refused it,
	// doubled after each refusal until recordTimeout runs out.
	recordRetry = 50 * time.Millisecond
	// maxErrorText bounds the failure text a delivery keeps.
	maxErrorText = 500
)

// Message is one event of a run on its way to one target, as a sender renders and delivers it.
type Message struct {
	// Target is the channel configuration, its secrets opened for this attempt only.
	Target run.NotifyTarget
	// Run is the run as it stood at the event.
	Run *run.Run
	// Event is started, success, failure, or approval.
	Event string
	// Seq is the event's position in the run's sequence.
	Seq int64
	// Delivery is the delivery's idempotency key, which a receiver can use to drop a repeat.
	Delivery string
	// Note is a sentence the message carries beyond the event, such as EarlierFailedNote, or empty.
	Note string
}

// SendFunc delivers one message. An error marked with Permanent is not retried. The error text is
// kept on the delivery and shown to operators, so it must not carry the target's address or key.
type SendFunc func(ctx context.Context, m Message) error

// Outbox records the events of top-level runs for the named targets attached to what they came
// from, and delivers them.
//
// Recording happens on the path of the run, in the order the run reaches its events, and each event
// takes the run's next sequence number in the same transaction that queues a delivery per target,
// so the order is the run's own and never a timestamp's. Delivery is per target and per run: a
// delivery waits until every earlier delivery to the same target for the same run has finished,
// delivered, skipped, or failed for good, except one on a different branch of a workflow, which
// happened concurrently with it. Targets wait on nothing of each other's: each has its own share of
// the attempts in flight, and a pass claims again as soon as any attempt ends. A delivery that
// fails is retried a bounded number of times and then marked failed, which is kept, and the
// deliveries after it go ahead carrying a note that an earlier one failed. An attempt the process
// cuts short by stopping is not counted against those retries. Every row is keyed by target, run,
// and sequence, so retries and several processes delivering at once never queue or claim one twice.
type Outbox struct {
	// store holds the events and deliveries.
	store Store
	// router finds the targets attached for an event.
	router *Router
	// sealer opens a target's secrets for one attempt.
	sealer Sealer
	// log records deliveries that failed for good, never a target's secrets.
	log *zap.Logger
	// owner names this process on the claims it takes.
	owner string
	// clock reads the time claims and retries are measured by.
	clock func(ctx context.Context) (time.Time, error)
	// wake nudges an idle Serve to look for work now. It holds one token.
	wake chan struct{}
	// poll is how often an idle Serve looks for work.
	poll time.Duration
	// lease is how long a claim lasts.
	lease time.Duration
	// batch bounds one claim.
	batch int
	// perTarget bounds the attempts at one target in flight at once.
	perTarget int
	// workers bounds all the attempts in flight at once.
	workers int
	// attemptTimeout bounds one attempt.
	attemptTimeout time.Duration
}

// OutboxOption configures an Outbox.
type OutboxOption func(*Outbox)

// WithClock reads the time claims and retries are measured by. Processes sharing one database pass
// the database's clock, so a claim one of them takes lapses at the same instant for all of them.
func WithClock(clock func(ctx context.Context) (time.Time, error)) OutboxOption {
	return func(o *Outbox) {
		if clock != nil {
			o.clock = clock
		}
	}
}

// WithOwner names this process on the claims it takes.
func WithOwner(owner string) OutboxOption {
	return func(o *Outbox) {
		if owner != "" {
			o.owner = owner
		}
	}
}

// WithPoll sets how often an idle outbox looks for deliveries that came due.
func WithPoll(d time.Duration) OutboxOption {
	return func(o *Outbox) {
		if d > 0 {
			o.poll = d
		}
	}
}

// NewOutbox returns an Outbox. It panics on a nil store or router, which is a wiring mistake; a nil
// logger is a no-op.
func NewOutbox(store Store, router *Router, sealer Sealer, log *zap.Logger, opts ...OutboxOption) *Outbox {
	if store == nil {
		panic("notification: Outbox needs a Store")
	}
	if router == nil {
		panic("notification: Outbox needs a Router")
	}
	if log == nil {
		log = zap.NewNop()
	}
	o := &Outbox{
		store: store, router: router, sealer: sealer, log: log,
		owner: idgen.New("outbox_", 6),
		clock: func(context.Context) (time.Time, error) { return time.Now(), nil },
		wake:  make(chan struct{}, 1), poll: defaultPoll, lease: defaultLease, batch: defaultBatch,
		perTarget: defaultPerTarget, workers: defaultWorkers, attemptTimeout: defaultAttemptTimeout,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Record records the event r is at for every target attached for it, as the run's next event, and
// wakes the delivery loop. r is the run as it stands at the event, already redacted for sending off
// the host. branch places the event within a workflow, and is the zero Branch for the run's own
// lifecycle. A run at no event, a run with no id, and an event no target is attached for record
// nothing.
//
// A store that refuses the write, or cannot be read to find the targets, is asked again with a
// growing wait until recordTimeout runs out, since one dropped connection or failed-over statement
// at the moment a run ends would otherwise leave its targets never told. An error means the event
// is not recorded, and a caller that can come back for it later, as the sweep of owed run ends
// does, should.
func (o *Outbox) Record(ctx context.Context, r *run.Run, branch Branch) error {
	if r == nil || r.ID == "" {
		return nil
	}
	event := EventOf(r)
	if event == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	snapshot, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("record notification event: %w", err)
	}
	for wait := recordRetry; ; wait *= 2 {
		recorded, err := o.record(ctx, r, event, branch, snapshot)
		if err == nil {
			if recorded {
				o.Wake()
			}
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("record notification event: %w", err)
		case <-timer.C:
		}
	}
}

// record makes one attempt at recording r's event for the targets that hear it, reporting whether
// the store recorded it.
func (o *Outbox) record(ctx context.Context, r *run.Run, event string, branch Branch,
	snapshot []byte) (bool, error) {
	attached, err := o.router.Recipients(ctx, r)
	if err != nil {
		return false, err
	}
	var recipients []Recipient
	for _, rc := range attached {
		if Hears(rc.Kind, r) {
			recipients = append(recipients, rc)
		}
	}
	if len(recipients) == 0 {
		return false, nil
	}
	ev := &RunEvent{RunID: r.ID, Event: event, Branch: branch.Step, Follows: branch.Follows,
		Snapshot: snapshot, CreatedAt: o.now(ctx)}
	return o.store.Record(ctx, ev, recipients)
}

// Hears reports whether a channel of the given kind is sent anything about a run where it stands. A
// hold asks for a decision rather than reporting an incident, so it never pages, texts, or
// annotates a dashboard, and a PagerDuty target pages only for a run that failed or was
// interrupted. An attention alert does report a problem, whatever the run's status, and a target
// is asked about one only when it was attached for the attention event, which is the request to
// hear it on that channel, a pager included. These are the rules every other path to these
// channels keeps, so a named target cannot be told what a template's own target of the same kind
// would not be.
func Hears(kind string, r *run.Run) bool {
	if r.Attention != nil {
		return true
	}
	if r.Status == run.StatusPendingApproval {
		switch kind {
		case run.NotifyPagerDuty, run.NotifyGrafana, run.NotifyTwilio:
			return false
		}
	}
	if kind == run.NotifyPagerDuty {
		return r.Status == run.StatusFailed || r.Status == run.StatusInterrupted
	}
	return true
}

// Wake nudges an idle Serve to look for work now rather than at its next poll.
func (o *Outbox) Wake() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Serve delivers recorded events through send until ctx ends. It returns once its last pass is
// done; a delivery that pass did not reach stays recorded and is delivered by the next process to
// serve, since every event lives in the store rather than in this process.
func (o *Outbox) Serve(ctx context.Context, send SendFunc) {
	if send == nil {
		panic("notification: Serve needs a SendFunc")
	}
	for ctx.Err() == nil {
		claimed := o.Flush(ctx, send)
		if claimed > 0 {
			continue
		}
		timer := time.NewTimer(o.poll)
		select {
		case <-ctx.Done():
		case <-o.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Flush makes one pass and returns how many deliveries it claimed. The pass attempts what is due
// and claims again whenever an attempt ends, an event is recorded, or the poll interval passes, so
// a target whose attempts hang holds only its own share of them while the deliveries due to every
// other target are claimed and attempted beside it. It returns once nothing it may claim is due and
// none of its attempts is still going. When ctx ends it claims nothing more and waits for the
// attempts in flight, which the end of ctx cuts short and which are released rather than counted.
// A server shutting down calls it once more after its runs have stopped, under a deadline, so the
// last events they recorded go out before the process exits when they can.
func (o *Outbox) Flush(ctx context.Context, send SendFunc) int {
	ended := make(chan struct{}, o.workers)
	inFlight, claimed := 0, 0
	for {
		if ctx.Err() == nil && inFlight < o.workers {
			batch := o.claim(ctx, min(o.batch, o.workers-inFlight))
			for _, d := range batch {
				inFlight++
				go func(d *Delivery) {
					defer func() { ended <- struct{}{} }()
					o.attempt(ctx, send, d)
				}(d)
			}
			claimed += len(batch)
			if len(batch) > 0 {
				continue
			}
		}
		if inFlight == 0 {
			return claimed
		}
		if ctx.Err() != nil {
			<-ended
			inFlight--
			continue
		}
		timer := time.NewTimer(o.poll)
		select {
		case <-ended:
			inFlight--
		case <-o.wake:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
}

// claim takes up to limit due deliveries for this process, at most perTarget to one target counting
// the ones it already holds, and logs a claim the store refused.
func (o *Outbox) claim(ctx context.Context, limit int) []*Delivery {
	claimed, err := o.store.Claim(ctx, o.owner, o.now(ctx), o.lease, limit, o.perTarget)
	if err != nil {
		if ctx.Err() == nil {
			o.log.Error("notification: claim deliveries: " + err.Error())
		}
		return nil
	}
	return claimed
}

// attempt makes one attempt at a claimed delivery and records what it came to: delivered, skipped
// because its target no longer hears the event, retried after a wait, or failed for good once the
// attempts run out or the failure is one no retry fixes. An attempt that failed because ctx ended,
// the process stopping rather than the target answering, is released uncounted instead, so a
// rolling restart never spends a delivery's last retry.
func (o *Outbox) attempt(ctx context.Context, send SendFunc, d *Delivery) {
	note := ""
	if d.EarlierFailed {
		note = EarlierFailedNote
	}
	err := o.deliver(ctx, send, d, note)
	// The outcome is recorded even when the process is shutting down, so an attempt that reached
	// the target is never retried for want of a record of it.
	rec, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err != nil && ctx.Err() != nil {
		if rerr := o.store.Release(rec, d, o.owner); rerr != nil {
			o.log.Error("notification: release delivery: "+rerr.Error(),
				zap.String("notification_id", d.NotificationID), zap.String("run_id", d.RunID),
				zap.Int64("seq", d.Seq))
		}
		return
	}
	at := o.now(rec)
	out := Outcome{Status: DeliveryDelivered, Note: note, At: at}
	var skip *skipError
	switch {
	case errors.As(err, &skip):
		out.Status, out.Error, out.Note = DeliverySkipped, skip.reason, ""
	case err != nil:
		out.Error = util.Clip(err.Error(), maxErrorText)
		switch {
		case IsPermanent(err) || d.Attempts+1 >= MaxAttempts:
			out.Status = DeliveryFailed
		default:
			out.Status = DeliveryPending
			out.NextAttemptAt = at.Add(RetryDelay(d.Attempts + 1))
		}
	}
	if ferr := o.store.Finish(rec, d, o.owner, out); ferr != nil {
		o.log.Error("notification: record delivery: "+ferr.Error(),
			zap.String("notification_id", d.NotificationID), zap.String("run_id", d.RunID),
			zap.Int64("seq", d.Seq))
		return
	}
	if out.Status == DeliveryFailed {
		o.log.Warn("notification: delivery failed and will not be retried: "+out.Error,
			zap.String("notification_id", d.NotificationID), zap.String("run_id", d.RunID),
			zap.Int64("seq", d.Seq), zap.String("event", d.Event),
			zap.Int("attempts", d.Attempts+1))
	}
}

// deliver opens the target and the run snapshot for one attempt and hands the message to send. A
// target that is gone, waiting for its secret, or whose secrets cannot be opened is a failure no
// retry fixes. A target edited since the event to a kind that event never reaches, a hold now
// pointed at a pager, is skipped: the channel rules are applied to the target as it stands at the
// attempt, which is the target the message would reach. The target's address and key are scrubbed
// from whatever the attempt reports, so a transport error that quotes them never reaches the
// record.
func (o *Outbox) deliver(ctx context.Context, send SendFunc, d *Delivery, note string) error {
	n, err := o.store.Get(ctx, d.NotificationID)
	if errors.Is(err, ErrNotFound) {
		return Permanent(errors.New("the target was deleted before this notification was " +
			"delivered"))
	}
	if err != nil {
		return fmt.Errorf("read target: %w", err)
	}
	var r run.Run
	if err := json.Unmarshal(d.Snapshot, &r); err != nil {
		return Permanent(fmt.Errorf("read the run as it stood at the event: %w", err))
	}
	if !Hears(n.Kind, &r) {
		return &skipError{reason: "not sent: the target was changed to " + n.Kind +
			", which this event does not reach"}
	}
	t, err := n.Target(o.sealer)
	if errors.Is(err, ErrNeedsSecret) {
		return Permanent(errors.New("the target is waiting for its secret to be entered"))
	}
	if err != nil {
		return Permanent(err)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, o.attemptTimeout)
	defer cancel()
	err = send(attemptCtx, Message{Target: t, Run: &r, Event: d.Event, Seq: d.Seq,
		Delivery: d.Key(), Note: note})
	if err != nil {
		err = scrubbed(err, t)
	}
	t.URL, t.Key = "", ""
	return err
}

// skipError is a delivery that will not be attempted because its target no longer hears the event.
type skipError struct {
	// reason says why, for the delivery's record.
	reason string
}

// Error returns the reason.
func (e *skipError) Error() string { return e.reason }

// scrubbed returns err with the target's address and key masked out of its text, keeping whether
// it is permanent.
func scrubbed(err error, t run.NotifyTarget) error {
	msg := err.Error()
	if t.URL != "" {
		msg = strings.ReplaceAll(msg, t.URL, util.MaskURL(t.URL))
	}
	if t.Key != "" {
		msg = strings.ReplaceAll(msg, t.Key, util.MaskMarker)
	}
	if msg == err.Error() {
		return err
	}
	if IsPermanent(err) {
		return Permanent(errors.New(msg))
	}
	return errors.New(msg)
}

// now reads the outbox clock, falling back to this process's clock when it cannot be read.
func (o *Outbox) now(ctx context.Context) time.Time {
	t, err := o.clock(ctx)
	if err != nil || t.IsZero() {
		return time.Now()
	}
	return t
}
