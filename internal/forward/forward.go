// Package forward streams the audit chain into the operator's SIEM, each event carrying the
// receipt that redeems it against the chain. Evidence nobody looks at is evidence in name only;
// the SIEM is where operators already look, and a receipt on every event means any sampled event
// can be held against the live chain with "switchtender audit receipt". The forwarder never
// invents, filters, or reorders: it is a cursor over the chain, delivered at least once.
package forward

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
)

// Event is one audit entry as the SIEM receives it. Receipt is the seq:link pair an auditor
// redeems against the chain, which is what makes a forwarded copy more than a copy.
type Event struct {
	// ID is the audit entry identifier.
	ID string `json:"id"`
	// At is when the entry was appended.
	At time.Time `json:"at"`
	// Actor, Method, and Path say who did what.
	Actor  string `json:"actor"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Seq is the entry's chain position.
	Seq int64 `json:"seq"`
	// Receipt is the redeemable seq:link pair.
	Receipt string `json:"receipt"`
}

// Sink delivers a batch of events somewhere durable. Deliver returns nil only when the batch was
// accepted; anything less is a failure the forwarder retries.
type Sink interface {
	// Deliver sends one batch, all or nothing as far as the caller is concerned.
	Deliver(ctx context.Context, events []Event) error
	// Name says which sink this is, for logs.
	Name() string
	// Close releases the sink's connection, if it holds one.
	Close() error
}

// Forwarder tails the chain and delivers every entry to every sink, advancing a durable cursor
// only when every sink accepted, so a crash or an outage redelivers rather than drops. Delivery
// is therefore at least once, which is the honest end of the trade: a SIEM can deduplicate on
// the receipt, but nothing can restore an event that was silently skipped.
type Forwarder struct {
	// audits is the chain being tailed.
	audits audit.Store
	// sinks receive every event; all must accept before the cursor advances.
	sinks []Sink
	// cursorPath is the durable cursor file.
	cursorPath string
	// cursor is the last delivered position held in memory; the file is durability, not the
	// per-poll source of truth. cursorLoaded is whether it has been read from the file and checked
	// against the chain yet, which happens once, at Start or on the first forward.
	cursor       cursorDoc
	cursorLoaded bool
	// interval is how long the tail sleeps when caught up.
	interval time.Duration
	// batch caps how many entries one delivery carries.
	batch int
	// log records forwarding activity.
	log *zap.Logger
	// ctx and cancel stop the loop; wg waits for it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// forwardBatch is how many entries one delivery carries at most. Bounded so a fresh forwarder
// over a long chain streams it in pages rather than one giant body.
const forwardBatch = 500

// NewForwarder returns a forwarder over the chain. It panics on a nil store, no sinks, an empty
// cursor path, or an interval under a second, all programming errors in the caller's wiring.
func NewForwarder(audits audit.Store, sinks []Sink, cursorPath string, interval time.Duration,
	log *zap.Logger) *Forwarder {
	if audits == nil {
		panic("forward: audit store required")
	}
	if len(sinks) == 0 {
		panic("forward: at least one sink required")
	}
	if cursorPath == "" {
		panic("forward: cursor path required")
	}
	if interval < time.Second {
		panic("forward: interval must be at least a second")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Forwarder{audits: audits, sinks: sinks, cursorPath: cursorPath, interval: interval,
		batch: forwardBatch, log: log, ctx: ctx, cancel: cancel}
}

// Start validates the cursor and launches the tail. The cursor file is read here rather than at
// the first delivery, so a corrupt cursor stops the server's startup loudly instead of silently
// restreaming the whole chain into the SIEM.
func (f *Forwarder) Start() error {
	if _, err := f.currentCursor(f.ctx); err != nil {
		return err
	}
	// A failed delivery is an outage to report, not a stack trace to print: the production logger
	// attaches one to every error line, and the cause is always the sink, never this code.
	quiet := f.log.WithOptions(zap.AddStacktrace(zap.DPanicLevel))
	f.wg.Go(func() {
		backoff := time.Second
		var down outage
		for {
			delivered, err := f.forwardOnce(f.ctx)
			if err == nil {
				if line := down.recovered(time.Now()); line != "" {
					f.log.Info(line)
				}
			}
			switch {
			case err != nil:
				if line := down.failed(time.Now(), err); line != "" {
					quiet.Error(line)
				}
				// Failure backs off to a bounded ceiling: hammering a down SIEM helps nobody,
				// and the cursor holds so nothing is lost, only late.
				if !f.sleep(backoff) {
					return
				}
				if backoff *= 2; backoff > time.Minute {
					backoff = time.Minute
				}
			case delivered > 0:
				// More may be waiting; drain without sleeping.
				backoff = time.Second
			default:
				backoff = time.Second
				if !f.sleep(f.interval) {
					return
				}
			}
		}
	})
	return nil
}

// outageReminder is how often a failure that has not changed is reported again while it lasts:
// often enough that a long outage stays in view, rarely enough that it does not bury everything else.
const outageReminder = 10 * time.Minute

// outage tracks a run of failed deliveries, so the log reports the outage rather than every retry.
// The loop retries with backoff to a one-minute ceiling and logged each attempt at error level with
// a stack trace, so an hour-long SIEM outage wrote dozens of identical multi-line entries.
type outage struct {
	// since is when this outage's first failure happened, zero while deliveries succeed.
	since time.Time
	// attempts counts the failed deliveries in this outage.
	attempts int
	// last is the failure most recently reported.
	last string
	// reported is when the outage was last reported.
	reported time.Time
}

// failed records one failed delivery and returns the line to log. It is empty for a failure that
// repeats the last one reported less than outageReminder ago.
func (o *outage) failed(now time.Time, err error) string {
	msg := err.Error()
	o.attempts++
	switch {
	case o.since.IsZero():
		o.since, o.last, o.reported = now, msg, now
		return "forward: delivery failed and will be retried with backoff. The cursor holds, so " +
			"nothing is skipped: " + msg
	case msg != o.last || now.Sub(o.reported) >= outageReminder:
		o.last, o.reported = msg, now
		return fmt.Sprintf("forward: delivery still failing after %d attempts over %s: %s",
			o.attempts, now.Sub(o.since).Round(time.Second), msg)
	default:
		return ""
	}
}

// recovered ends the outage and returns the line to log, empty when there was none.
func (o *outage) recovered(now time.Time) string {
	if o.since.IsZero() {
		return ""
	}
	line := fmt.Sprintf("forward: delivery resumed after %d failed attempts over %s",
		o.attempts, now.Sub(o.since).Round(time.Second))
	*o = outage{}
	return line
}

// sleep waits d or until the forwarder stops, reporting whether to keep running.
func (f *Forwarder) sleep(d time.Duration) bool {
	select {
	case <-f.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// currentCursor returns the last delivered position, loading it from the file once and holding it in
// memory thereafter. The forwarder is the file's only writer, so re-reading and re-parsing it every
// poll bought nothing; loading once and trusting memory keeps the tail's fast path off the disk while
// the file stays the durable record a restart resumes from.
//
// The loaded position is checked against the chain before it is trusted. A database restored from an
// older copy, or one whose tail was cut, no longer holds the entry the cursor names, and the cursor
// stayed ahead of it: the entries the rolled-back chain appended at sequences the cursor had already
// passed were never forwarded, nothing was logged, and the SIEM held the old entries spliced to the
// new ones. The forwarder now resumes after the newest delivered position the chain still holds, and
// says so. What it sends again carries the same receipt, which is the SIEM's deduplication key.
func (f *Forwarder) currentCursor(ctx context.Context) (cursorDoc, error) {
	if f.cursorLoaded {
		return f.cursor, nil
	}
	doc, err := readCursor(f.cursorPath)
	if err != nil {
		return cursorDoc{}, err
	}
	resumed, err := f.reconcile(ctx, doc)
	if err != nil {
		return cursorDoc{}, err
	}
	if resumed.Seq != doc.Seq {
		f.log.Warn("forward: the audit chain no longer holds the last entry this forwarder delivered, "+
			"so it was rolled back or replaced. Resuming after the newest delivered entry it still "+
			"holds. The SIEM may hold entries this chain no longer has",
			zap.Int64("delivered_through", doc.Seq), zap.Int64("resuming_after", resumed.Seq))
		if err := writeCursor(f.cursorPath, resumed); err != nil {
			return cursorDoc{}, fmt.Errorf("rewind cursor: %w", err)
		}
	}
	f.cursor, f.cursorLoaded = resumed, true
	return resumed, nil
}

// reconcile returns the newest delivered position the chain still holds: the cursor itself when the
// chain holds its entry unchanged, otherwise the newest checkpoint whose entry it holds, or the start
// of the chain when it holds none of them. A cursor written before links were kept cannot be checked
// against a changed entry, only against a chain cut below it.
func (f *Forwarder) reconcile(ctx context.Context, doc cursorDoc) (cursorDoc, error) {
	if doc.Seq == 0 {
		return doc, nil
	}
	head, err := f.linkAt(ctx, doc.Seq)
	if err != nil {
		return cursorDoc{}, err
	}
	if head != "" && (doc.Link == "" || head == doc.Link) {
		return doc, nil
	}
	for i := len(doc.Checkpoints) - 1; i >= 0; i-- {
		cp := doc.Checkpoints[i]
		held, err := f.linkAt(ctx, cp.Seq)
		if err != nil {
			return cursorDoc{}, err
		}
		if held != "" && held == cp.Link {
			return cursorDoc{Seq: cp.Seq, Link: cp.Link, Checkpoints: doc.Checkpoints[:i]}, nil
		}
	}
	return cursorDoc{}, nil
}

// linkAt returns the link of the entry at seq, or the empty string when the chain holds no entry
// there.
func (f *Forwarder) linkAt(ctx context.Context, seq int64) (string, error) {
	link := ""
	err := f.audits.ChainScan(ctx, seq-1, func(e *audit.Entry) error {
		if e.Seq == seq {
			link = e.Hash
		}
		return errBatchFull
	})
	if err != nil && err != errBatchFull {
		return "", fmt.Errorf("read the delivered entry at seq %d: %w", seq, err)
	}
	return link, nil
}

// forwardOnce reads one batch past the cursor, delivers it to every sink, and advances the
// cursor. It returns how many entries were delivered.
func (f *Forwarder) forwardOnce(ctx context.Context) (int, error) {
	cursor, err := f.currentCursor(ctx)
	if err != nil {
		return 0, err
	}
	events, link, err := f.readBatch(ctx, cursor.Seq)
	if err != nil {
		return 0, fmt.Errorf("read chain: %w", err)
	}
	if len(events) == 0 {
		return 0, nil
	}
	for _, sink := range f.sinks {
		if err := sink.Deliver(ctx, events); err != nil {
			// The cursor stands, so the whole batch is redelivered next round, to every sink.
			// A sink that already accepted sees the batch again; at least once is the contract,
			// and the receipt is the deduplication key.
			return 0, fmt.Errorf("%s: %w", sink.Name(), err)
		}
	}
	// The file is written first: a crash after this line but before the in-memory update simply
	// re-reads the same position on restart, so durability leads and memory follows it.
	next := cursor.advance(events[len(events)-1].Seq, link)
	if err := writeCursor(f.cursorPath, next); err != nil {
		return 0, fmt.Errorf("advance cursor: %w", err)
	}
	f.cursor = next
	return len(events), nil
}

// errBatchFull stops a chain scan once the batch is full.
var errBatchFull = fmt.Errorf("batch full")

// readBatch reads up to batch entries after seq, in chain order, and the link of the last one.
func (f *Forwarder) readBatch(ctx context.Context, seq int64) ([]Event, string, error) {
	var events []Event
	link := ""
	err := f.audits.ChainScan(ctx, seq, func(e *audit.Entry) error {
		events = append(events, Event{
			ID: e.ID, At: e.At, Actor: e.Actor, Method: e.Method, Path: e.Path,
			Seq: e.Seq, Receipt: audit.Receipt(e),
		})
		link = e.Hash
		if len(events) >= f.batch {
			return errBatchFull
		}
		return nil
	})
	if err != nil && err != errBatchFull {
		return nil, "", err
	}
	return events, link, nil
}

// Close stops the tail, waits for it, and closes every sink.
func (f *Forwarder) Close() {
	f.cancel()
	f.wg.Wait()
	for _, sink := range f.sinks {
		if err := sink.Close(); err != nil {
			f.log.Error("forward: close " + sink.Name() + ": " + err.Error())
		}
	}
}

// maxCheckpoints bounds the earlier positions a cursor keeps.
const maxCheckpoints = 64

// cursorDoc is the durable cursor's file format.
type cursorDoc struct {
	// Seq is the last sequence every sink accepted.
	Seq int64 `json:"seq"`
	// Link is the link of the entry at Seq, so a chain changed under the cursor is noticed.
	Link string `json:"link,omitempty"`
	// Checkpoints are earlier delivered positions, oldest first and sparser the older they are, the
	// places a rolled-back chain can resume from without sending the whole chain again.
	Checkpoints []checkpoint `json:"checkpoints,omitempty"`
}

// checkpoint is one delivered position.
type checkpoint struct {
	// Seq is the delivered entry's sequence.
	Seq int64 `json:"seq"`
	// Link is the delivered entry's link.
	Link string `json:"link"`
}

// advance returns the cursor moved to the entry at seq with the given link, keeping the position it
// leaves as a checkpoint. Past the bound, the older half is thinned by dropping every other
// checkpoint, so the kept positions reach further back the older they are rather than covering only
// the last few batches.
func (c cursorDoc) advance(seq int64, link string) cursorDoc {
	kept := append([]checkpoint(nil), c.Checkpoints...)
	if c.Seq > 0 && c.Link != "" {
		kept = append(kept, checkpoint{Seq: c.Seq, Link: c.Link})
	}
	if len(kept) > maxCheckpoints {
		half := len(kept) / 2
		thinned := make([]checkpoint, 0, maxCheckpoints)
		for i := 0; i < half; i += 2 {
			thinned = append(thinned, kept[i])
		}
		kept = append(thinned, kept[half:]...)
	}
	return cursorDoc{Seq: seq, Link: link, Checkpoints: kept}
}

// readCursor returns the last delivered position, the zero position when the file does not exist yet.
// A file that exists but cannot be parsed is an error, never a silent restart from zero: restreaming a
// whole chain because of a corrupt byte would flood the SIEM with years of duplicates.
func readCursor(path string) (cursorDoc, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cursorDoc{}, nil
	}
	if err != nil {
		return cursorDoc{}, fmt.Errorf("read forward cursor: %w", err)
	}
	var doc cursorDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return cursorDoc{}, fmt.Errorf("parse forward cursor %s: %w", path, err)
	}
	return doc, nil
}

// writeCursor records the last delivered position, atomically so a crash mid-write leaves the
// previous cursor rather than a truncated file.
func writeCursor(path string, doc cursorDoc) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
