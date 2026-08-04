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
	if _, err := readCursor(f.cursorPath); err != nil {
		return err
	}
	f.wg.Go(func() {
		backoff := time.Second
		for {
			delivered, err := f.forwardOnce(f.ctx)
			switch {
			case err != nil:
				f.log.Error("forward: " + err.Error())
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

// sleep waits d or until the forwarder stops, reporting whether to keep running.
func (f *Forwarder) sleep(d time.Duration) bool {
	select {
	case <-f.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// forwardOnce reads one batch past the cursor, delivers it to every sink, and advances the
// cursor. It returns how many entries were delivered.
func (f *Forwarder) forwardOnce(ctx context.Context) (int, error) {
	cursor, err := readCursor(f.cursorPath)
	if err != nil {
		return 0, err
	}
	events, err := f.readBatch(ctx, cursor)
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
	if err := writeCursor(f.cursorPath, events[len(events)-1].Seq); err != nil {
		return 0, fmt.Errorf("advance cursor: %w", err)
	}
	return len(events), nil
}

// errBatchFull stops a chain scan once the batch is full.
var errBatchFull = fmt.Errorf("batch full")

// readBatch reads up to batch entries after seq, in chain order.
func (f *Forwarder) readBatch(ctx context.Context, seq int64) ([]Event, error) {
	var events []Event
	err := f.audits.ChainScan(ctx, seq, func(e *audit.Entry) error {
		events = append(events, Event{
			ID: e.ID, At: e.At, Actor: e.Actor, Method: e.Method, Path: e.Path,
			Seq: e.Seq, Receipt: audit.Receipt(e),
		})
		if len(events) >= f.batch {
			return errBatchFull
		}
		return nil
	})
	if err != nil && err != errBatchFull {
		return nil, err
	}
	return events, nil
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

// cursorDoc is the durable cursor's file format.
type cursorDoc struct {
	// Seq is the last sequence every sink accepted.
	Seq int64 `json:"seq"`
}

// readCursor returns the last delivered sequence, zero when the file does not exist yet. A file
// that exists but cannot be parsed is an error, never a silent restart from zero: restreaming a
// whole chain because of a corrupt byte would flood the SIEM with years of duplicates.
func readCursor(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read forward cursor: %w", err)
	}
	var doc cursorDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, fmt.Errorf("parse forward cursor %s: %w", path, err)
	}
	return doc.Seq, nil
}

// writeCursor records the last delivered sequence, atomically so a crash mid-write leaves the
// previous cursor rather than a truncated file.
func writeCursor(path string, seq int64) error {
	data, err := json.Marshal(cursorDoc{Seq: seq})
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
