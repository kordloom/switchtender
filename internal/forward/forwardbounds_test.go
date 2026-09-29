package forward

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestNewForwarderRefusesBrokenWiring proves the constructor fails loudly on wiring a caller got
// wrong. Every one of these produces a forwarder that looks alive and forwards nothing: no store to
// read, no sink to deliver to, no file to remember how far it got, or a poll interval that spins.
// Silence is the worst outcome for a component whose whole job is that somebody is watching, so the
// failure has to arrive at startup rather than as an empty SIEM nobody notices.
func TestNewForwarderRefusesBrokenWiring(t *testing.T) {
	t.Parallel()
	audits := seedAudits(t, 1)
	dir := t.TempDir()
	tests := []struct {
		// Name labels the mistake.
		Name string
		// Audits is the chain to tail.
		Audits audit.Store
		// Sinks receive the events.
		Sinks []Sink
		// CursorPath is the durable cursor file.
		CursorPath string
		// Interval is how long the tail sleeps when caught up.
		Interval time.Duration
		// WantPanic is the message fragment the panic must carry.
		WantPanic string
	}{{ // Test 0: No chain to read means nothing to forward, forever.
		Name: "nil store", Audits: nil, Sinks: []Sink{&captureSink{}},
		CursorPath: filepath.Join(dir, "a"), Interval: time.Second,
		WantPanic: "audit store required",
	}, { // Test 1: No sink means every event is read and discarded.
		Name: "no sinks", Audits: audits, Sinks: nil,
		CursorPath: filepath.Join(dir, "b"), Interval: time.Second,
		WantPanic: "at least one sink required",
	}, { // Test 2: An empty sink slice is the same mistake spelled differently.
		Name: "empty sink slice", Audits: audits, Sinks: []Sink{},
		CursorPath: filepath.Join(dir, "c"), Interval: time.Second,
		WantPanic: "at least one sink required",
	}, { // Test 3: With no cursor file a restart restreams the whole chain into the SIEM.
		Name: "empty cursor path", Audits: audits, Sinks: []Sink{&captureSink{}},
		CursorPath: "", Interval: time.Second, WantPanic: "cursor path required",
	}, { // Test 4: A zero interval turns the caught-up tail into a spin loop.
		Name: "zero interval", Audits: audits, Sinks: []Sink{&captureSink{}},
		CursorPath: filepath.Join(dir, "d"), Interval: 0,
		WantPanic: "interval must be at least a second",
	}, { // Test 5: So does anything under a second.
		Name: "sub second interval", Audits: audits, Sinks: []Sink{&captureSink{}},
		CursorPath: filepath.Join(dir, "e"), Interval: 999 * time.Millisecond,
		WantPanic: "interval must be at least a second",
	}, { // Test 6: A negative interval is the same class of mistake.
		Name: "negative interval", Audits: audits, Sinks: []Sink{&captureSink{}},
		CursorPath: filepath.Join(dir, "f"), Interval: -time.Hour,
		WantPanic: "interval must be at least a second",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("NewForwarder() with %s did not panic, so the server starts with a "+
						"forwarder that silently forwards nothing", test.Name)
				}
				if msg, _ := r.(string); !strings.Contains(msg, test.WantPanic) {
					t.Errorf("panic = %v, want it to name %q", r, test.WantPanic)
				}
			}()
			NewForwarder(test.Audits, test.Sinks, test.CursorPath, test.Interval, nil)
		})
	}
}

// TestNewForwarderAcceptsAMinimalWiring pins the edges the constructor must accept: exactly the
// minimum interval, and no logger. A nil logger is a caller choice, not a mistake, so it must
// become a no-op rather than a nil dereference on the first delivery failure.
func TestNewForwarderAcceptsAMinimalWiring(t *testing.T) {
	t.Parallel()
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(seedAudits(t, 1), []Sink{&captureSink{}}, cursor, time.Second, nil)
	defer f.Close()
	if f.log == nil {
		t.Fatal("NewForwarder() left a nil logger, so the first failure panics the tail")
	}
	if f.batch != forwardBatch {
		t.Errorf("batch = %d, want the bounded page size %d", f.batch, forwardBatch)
	}
	// The failure path is the one that touches the logger, so exercise it.
	f.sinks = []Sink{&captureSink{refuse: true}}
	if _, err := f.forwardOnce(context.Background()); err == nil {
		t.Error("forwardOnce() with a refusing sink = nil error")
	}
	f.log.Error("forward: a delivery failed")
}

// TestReadCursorShapes pins how a cursor file is read, because the alternative to reading it
// correctly is restreaming years of chain into a SIEM. A missing file is a first start and reads as
// zero; a file that exists but cannot be parsed is an error, never a silent zero.
func TestReadCursorShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the file's state.
		Name string
		// Write is the file's content, nil to leave the file absent.
		Write []byte
		// WantSeq is the sequence read.
		WantSeq int64
		// WantErr is whether reading must fail rather than answer zero.
		WantErr bool
	}{{ // Test 0: No file yet is a first start, which legitimately reads as zero.
		Name: "absent", Write: nil, WantSeq: 0,
	}, { // Test 1: The ordinary case.
		Name: "normal", Write: []byte(`{"seq":42}`), WantSeq: 42,
	}, { // Test 2: Zero written explicitly is still zero.
		Name: "explicit zero", Write: []byte(`{"seq":0}`), WantSeq: 0,
	}, { // Test 3: A large sequence survives, since the chain outlives any int32 idea of one.
		Name: "max int64", Write: []byte(fmt.Sprintf(`{"seq":%d}`, int64(math.MaxInt64))),
		WantSeq: math.MaxInt64,
	}, { // Test 4: A field the format gained later is ignored rather than refused.
		Name: "extra field", Write: []byte(`{"seq":7,"written_at":"2026-08-04"}`), WantSeq: 7,
	}, { // Test 5: An empty file is corruption, not a first start.
		Name: "empty file", Write: []byte(``), WantErr: true,
	}, { // Test 6: Truncated JSON is corruption.
		Name: "truncated", Write: []byte(`{"seq":4`), WantErr: true,
	}, { // Test 7: Garbage is corruption.
		Name: "garbage", Write: []byte("\x00\x01binary"), WantErr: true,
	}, { // Test 8: A JSON value of the wrong type is corruption.
		Name: "wrong type", Write: []byte(`{"seq":"forty-two"}`), WantErr: true,
	}, { // Test 9: A JSON array is not this document.
		Name: "array", Write: []byte(`[1,2,3]`), WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cursor.json")
			if test.Write != nil {
				if err := os.WriteFile(path, test.Write, 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			seq, err := readCursor(path)
			if gotErr := err != nil; gotErr != test.WantErr {
				t.Fatalf("readCursor() error = %v, want an error: %v", err, test.WantErr)
			}
			if test.WantErr {
				return
			}
			if seq != test.WantSeq {
				t.Errorf("readCursor() = %d, want %d", seq, test.WantSeq)
			}
		})
	}
}

// TestReadCursorRefusesAnUnreadableFile proves a path that is not a readable file is an error
// rather than a zero. A directory where the cursor should be is a deployment mistake, and answering
// zero would turn it into a full replay of the chain on every restart.
func TestReadCursorRefusesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cursor.json")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if _, err := readCursor(dir); err == nil {
		t.Error("readCursor() on a directory = nil error, want the misconfiguration surfaced")
	}
}

// TestCursorRoundTrip pins that what is written is what comes back, at the edges a sequence can
// reach. The cursor is the only thing standing between a restart and a duplicate replay of the
// whole chain, so a value that does not survive the trip costs the operator a flooded SIEM.
func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Seq is the sequence to store.
		Seq int64
	}{
		{Seq: 0},             // Test 0: Nothing delivered yet.
		{Seq: 1},             // Test 1: The first entry.
		{Seq: 500},           // Test 2: One full page.
		{Seq: math.MaxInt64}, // Test 3: The largest a chain position can be.
		{Seq: math.MinInt64}, // Test 4: Never produced, but it must not corrupt the file.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cursor.json")
			if err := writeCursor(path, test.Seq); err != nil {
				t.Fatalf("writeCursor() error = %v", err)
			}
			got, err := readCursor(path)
			if err != nil {
				t.Fatalf("readCursor() error = %v", err)
			}
			if got != test.Seq {
				t.Errorf("readCursor() = %d, want the %d that was written", got, test.Seq)
			}
			if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
				t.Errorf("the temporary file survived the write, stat error = %v", err)
			}
		})
	}
}

// TestWriteCursorLeavesThePreviousValueOnFailure proves a failed write does not destroy the cursor
// that was already there. The write is the durability step for at-least-once delivery: losing the
// old value would restream the chain from wherever the damaged file happened to parse to.
func TestWriteCursorLeavesThePreviousValueOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")
	if err := writeCursor(path, 11); err != nil {
		t.Fatalf("writeCursor() error = %v", err)
	}
	// A path that cannot be written to at all, so the temporary write fails before the rename.
	missing := filepath.Join(dir, "no-such-directory", "cursor.json")
	if err := writeCursor(missing, 12); err == nil {
		t.Error("writeCursor() into a missing directory = nil error")
	}
	if seq, err := readCursor(path); err != nil || seq != 11 {
		t.Errorf("the existing cursor reads %d, %v, want the previous value 11 intact", seq, err)
	}
}

// TestForwardOnceHoldsTheCursorWhenTheWriteFails proves the delivered batch is redelivered when the
// cursor cannot be recorded. Advancing in memory after a failed durable write would mean the batch
// is dropped from this process's view and never replayed by the next one, which is the one thing an
// at-least-once forwarder must not do.
func TestForwardOnceHoldsTheCursorWhenTheWriteFails(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	unwritable := filepath.Join(t.TempDir(), "no-such-directory", "cursor.json")
	f := NewForwarder(seedAudits(t, 3), []Sink{sink}, unwritable, time.Second, nil)

	n, err := f.forwardOnce(context.Background())
	if err == nil {
		t.Fatal("forwardOnce() with an unwritable cursor = nil error, want the failure surfaced")
	}
	if !strings.Contains(err.Error(), "advance cursor") {
		t.Errorf("error = %v, want it to name the cursor write", err)
	}
	if n != 0 {
		t.Errorf("forwardOnce() reported %d delivered, want zero on a failed advance", n)
	}
	if f.cursor != 0 {
		t.Errorf("in-memory cursor = %d, want it held so the batch is redelivered", f.cursor)
	}
	if _, err := f.forwardOnce(context.Background()); err == nil {
		t.Error("the second attempt succeeded, so the cursor moved without being recorded")
	}
	if got := len(sink.all()); got != 6 {
		t.Errorf("the sink saw %d events over two attempts, want the same three twice", got)
	}
}

// errChainRead stands for a store that cannot answer, so a test can tell a read failure from an
// empty chain.
var errChainRead = errors.New("the database is gone")

// failingScanStore is an audit store whose chain cannot be read.
type failingScanStore struct {
	audit.Store
}

// ChainScan refuses, the way a store does when its database is unreachable.
func (failingScanStore) ChainScan(context.Context, int64, func(*audit.Entry) error) error {
	return errChainRead
}

// TestForwardOnceSurfacesAChainReadFailure proves an unreadable chain is an error the tail backs
// off on rather than a caught-up forwarder. A store that cannot answer and a chain with nothing new
// both produce no events, and treating the first as the second would leave the SIEM quietly empty
// while the server looked healthy.
func TestForwardOnceSurfacesAChainReadFailure(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(failingScanStore{audit.NewMemStore()}, []Sink{sink}, cursor, time.Second, nil)

	n, err := f.forwardOnce(context.Background())
	if !errors.Is(err, errChainRead) {
		t.Fatalf("forwardOnce() error = %v, want the store's failure reported", err)
	}
	if !strings.Contains(err.Error(), "read chain") {
		t.Errorf("error = %v, want it to name the chain read", err)
	}
	if n != 0 {
		t.Errorf("forwardOnce() reported %d delivered, want zero", n)
	}
	if len(sink.all()) != 0 {
		t.Error("a sink was given events despite the chain read failing")
	}
	if _, err := os.Stat(cursor); !os.IsNotExist(err) {
		t.Errorf("a cursor file was written for a batch that was never read, stat error = %v", err)
	}
}

// TestForwardOnceRefusesACorruptCursorBeforeReading proves the corrupt-cursor guard runs ahead of
// any delivery. Reading the chain first and failing afterward would already have loaded the whole
// trail, and a guard that only fires at startup would not protect a forwarder that was started
// before the file was damaged.
func TestForwardOnceRefusesACorruptCursorBeforeReading(t *testing.T) {
	t.Parallel()
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	if err := os.WriteFile(cursor, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sink := &captureSink{}
	f := NewForwarder(seedAudits(t, 3), []Sink{sink}, cursor, time.Second, nil)
	if _, err := f.forwardOnce(context.Background()); err == nil {
		t.Fatal("forwardOnce() over a corrupt cursor = nil error")
	}
	if len(sink.all()) != 0 {
		t.Error("events were delivered despite the cursor being unreadable")
	}
}

// TestForwarderDeliversToEverySinkInOrder proves a batch reaches all of the configured sinks, not
// just the first, and that each gets the same events. An operator forwarding to both a SIEM and a
// cold archive is relying on this; a sink quietly skipped is an archive with holes in it.
func TestForwarderDeliversToEverySinkInOrder(t *testing.T) {
	t.Parallel()
	first, second, third := &captureSink{}, &captureSink{}, &captureSink{}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(seedAudits(t, 4), []Sink{first, second, third}, cursor, time.Second, nil)
	if n, err := f.forwardOnce(context.Background()); err != nil || n != 4 {
		t.Fatalf("forwardOnce() = %d, %v, want the four entries", n, err)
	}
	for i, sink := range []*captureSink{first, second, third} {
		if diff := cmp.Diff(first.all(), sink.all()); diff != "" {
			t.Errorf("sink %d saw a different batch (-first +this):\n%s", i, diff)
		}
	}
}

// closeErrorSink is a sink whose Close fails, the shape a collector connection takes when it has
// already gone away.
type closeErrorSink struct {
	captureSink
	// closes counts how many times Close was called.
	closes int
}

// Close reports a failure the forwarder has to survive.
func (c *closeErrorSink) Close() error {
	c.closes++
	return errors.New("the collector connection was already gone")
}

// Name names the sink.
func (c *closeErrorSink) Name() string { return "closer" }

// TestForwarderCloseClosesEverySinkDespiteAFailure proves shutdown does not stop at the first sink
// that fails to close. A leaked connection per restart is how a long-lived server runs out of file
// descriptors, and the sink that failed is the one least likely to be the last in the list.
func TestForwarderCloseClosesEverySinkDespiteAFailure(t *testing.T) {
	t.Parallel()
	bad, good := &closeErrorSink{}, &captureSink{}
	buf := &logBuffer{}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(seedAudits(t, 1), []Sink{bad, good}, cursor, time.Second, captureLogger(buf))
	if err := f.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	f.Close()

	if bad.closes != 1 {
		t.Errorf("the failing sink was closed %d times, want once", bad.closes)
	}
	if logged := buf.String(); !strings.Contains(logged, "close closer") {
		t.Errorf("the close failure was not logged, got:\n%s", logged)
	}
}

// TestForwarderStartAndCloseAreSafeInEveryOrder pins the lifecycle a server actually drives, where
// a forwarder may be closed without ever having started and closed again on a second shutdown path.
// A panic here takes the whole process down during shutdown, when there is nothing left to report
// it.
func TestForwarderStartAndCloseAreSafeInEveryOrder(t *testing.T) {
	t.Parallel()

	t.Run("test 0", func(t *testing.T) { // Test 0: Closed without ever being started.
		t.Parallel()
		f := NewForwarder(seedAudits(t, 1), []Sink{&captureSink{}},
			filepath.Join(t.TempDir(), "cursor.json"), time.Second, nil)
		f.Close()
	})

	t.Run("test 1", func(t *testing.T) { // Test 1: Started, then closed twice.
		t.Parallel()
		f := NewForwarder(seedAudits(t, 1), []Sink{&captureSink{}},
			filepath.Join(t.TempDir(), "cursor.json"), time.Second, nil)
		if err := f.Start(); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		f.Close()
		f.Close()
	})

	t.Run("test 2", func(t *testing.T) { // Test 2: A refused start still closes cleanly.
		t.Parallel()
		cursor := filepath.Join(t.TempDir(), "cursor.json")
		if err := os.WriteFile(cursor, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		f := NewForwarder(seedAudits(t, 1), []Sink{&captureSink{}}, cursor, time.Second, nil)
		if err := f.Start(); err == nil {
			t.Fatal("Start() over a corrupt cursor = nil error")
		}
		f.Close()
	})
}

// TestForwarderTailDeliversAndStopsPromptly proves the running tail is what actually moves the
// chain, not just the single step a test can call directly, and that Close stops it rather than
// waiting out the poll interval. A shutdown that blocks for the interval turns every restart into a
// visible outage.
func TestForwarderTailDeliversAndStopsPromptly(t *testing.T) {
	t.Parallel()
	audits := seedAudits(t, 3)
	sink := &captureSink{}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(audits, []Sink{sink}, cursor, time.Hour, nil)
	if err := f.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.all()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(sink.all()); got != 3 {
		t.Fatalf("the tail delivered %d events, want the three on the chain", got)
	}

	stopped := make(chan struct{})
	go func() {
		f.Close()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return, so the tail ignores the stop signal while sleeping")
	}
	if seq, err := readCursor(cursor); err != nil || seq == 0 {
		t.Errorf("cursor = %d, %v, want it advanced past the delivered batch", seq, err)
	}
}

// TestForwarderSleepStopsOnClose pins the wait that both the poll interval and the failure backoff
// go through. It has to answer the stop signal rather than the timer, or a forwarder backing off a
// minute against a down collector holds the whole shutdown for a minute.
func TestForwarderSleepStopsOnClose(t *testing.T) {
	t.Parallel()
	f := NewForwarder(seedAudits(t, 1), []Sink{&captureSink{}},
		filepath.Join(t.TempDir(), "cursor.json"), time.Second, nil)
	done := make(chan bool, 1)
	go func() { done <- f.sleep(time.Hour) }()
	f.cancel()
	select {
	case keepRunning := <-done:
		if keepRunning {
			t.Error("sleep() reported keep running after the forwarder was stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sleep() waited out its timer instead of answering the stop signal")
	}
	if got := f.sleep(time.Millisecond); got {
		t.Error("sleep() after a stop reported keep running")
	}
}

// TestReadBatchStopsAtTheBatchSize pins the page boundary exactly, including the off-by-one at a
// chain holding one entry more or less than a full page. A fresh forwarder over a long chain
// streams it in pages rather than building one giant body, and the sentinel that stops the scan
// must not escape as an error.
func TestReadBatchStopsAtTheBatchSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Entries is how many the chain holds.
		Entries int
		// Batch is the page size.
		Batch int
		// WantCount is how many one read returns.
		WantCount int
	}{
		{Entries: 0, Batch: 3, WantCount: 0}, // Test 0: An empty chain.
		{Entries: 1, Batch: 3, WantCount: 1}, // Test 1: Under a page.
		{Entries: 2, Batch: 3, WantCount: 2}, // Test 2: One short of a page.
		{Entries: 3, Batch: 3, WantCount: 3}, // Test 3: Exactly a page.
		{Entries: 4, Batch: 3, WantCount: 3}, // Test 4: One over, so the sentinel fires.
		{Entries: 9, Batch: 3, WantCount: 3}, // Test 5: Several pages waiting.
		{Entries: 5, Batch: 1, WantCount: 1}, // Test 6: The smallest useful page.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := NewForwarder(seedAudits(t, test.Entries), []Sink{&captureSink{}},
				filepath.Join(t.TempDir(), "cursor.json"), time.Second, nil)
			f.batch = test.Batch
			events, err := f.readBatch(context.Background(), 0)
			if err != nil {
				t.Fatalf("readBatch() error = %v, want the batch-full sentinel absorbed", err)
			}
			if len(events) != test.WantCount {
				t.Fatalf("readBatch() returned %d events, want %d", len(events), test.WantCount)
			}
			for i := 1; i < len(events); i++ {
				if events[i].Seq <= events[i-1].Seq {
					t.Fatalf("events are out of chain order at %d: %d then %d",
						i, events[i-1].Seq, events[i].Seq)
				}
			}
		})
	}
}

// TestReadBatchCarriesTheRedeemableReceipt proves every forwarded event carries the seq:link pair
// that redeems it against the live chain. That receipt is what makes a copy in somebody else's SIEM
// more than a copy: without it a sampled event proves nothing, and the forwarder's whole reason for
// existing goes with it.
func TestReadBatchCarriesTheRedeemableReceipt(t *testing.T) {
	t.Parallel()
	audits := seedAudits(t, 5)
	f := NewForwarder(audits, []Sink{&captureSink{}},
		filepath.Join(t.TempDir(), "cursor.json"), time.Second, nil)
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	events, err := f.readBatch(context.Background(), 0)
	if err != nil {
		t.Fatalf("readBatch() error = %v", err)
	}
	for i, e := range events {
		if e.Receipt == "" {
			t.Fatalf("event %d carries no receipt, so it redeems against nothing", i)
		}
		if diff := cmp.Diff(audit.Receipt(chain[i]), e.Receipt); diff != "" {
			t.Errorf("event %d receipt mismatch (-want +got):\n%s", i, diff)
		}
		if e.ID != chain[i].ID || e.Actor != chain[i].Actor || e.Path != chain[i].Path {
			t.Errorf("event %d = %+v, want the entry's own fields", i, e)
		}
	}

	// Reading from a cursor past the end is the caught-up case and must be empty, not a replay.
	after, err := f.readBatch(context.Background(), chain[len(chain)-1].Seq)
	if err != nil || len(after) != 0 {
		t.Errorf("readBatch() past the head = %d events, %v, want nothing", len(after), err)
	}
}

// blockingSink holds a delivery until released, so a test can observe the forwarder while a batch
// is in flight.
type blockingSink struct {
	// mu guards seen.
	mu sync.Mutex
	// seen counts deliveries.
	seen int
	// release is closed to let a delivery finish.
	release chan struct{}
}

// Deliver waits for the test to release it. It deliberately ignores the context, the way a sink
// already inside a socket write does, so the test observes what Close does about a delivery it
// cannot interrupt.
func (b *blockingSink) Deliver(context.Context, []Event) error {
	b.mu.Lock()
	b.seen++
	b.mu.Unlock()
	<-b.release
	return nil
}

// Name names the sink.
func (b *blockingSink) Name() string { return "blocking" }

// Close does nothing.
func (b *blockingSink) Close() error { return nil }

// TestForwarderCloseWaitsForAnInFlightDelivery proves shutdown does not abandon a batch mid-flight
// and leave the tail goroutine writing to a closed sink. Close cancels, waits for the loop, and
// only then closes the sinks, which is the order that keeps a sink from being used after it is
// closed.
func TestForwarderCloseWaitsForAnInFlightDelivery(t *testing.T) {
	t.Parallel()
	sink := &blockingSink{release: make(chan struct{})}
	f := NewForwarder(seedAudits(t, 2), []Sink{sink},
		filepath.Join(t.TempDir(), "cursor.json"), time.Second, nil)
	if err := f.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		started := sink.seen > 0
		sink.mu.Unlock()
		if started {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	closed := make(chan struct{})
	go func() {
		f.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close() returned while a delivery was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(sink.release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() never returned after the delivery finished")
	}
}

// TestWriteCursorCleansUpWhenTheRenameFails proves a failed rename leaves no temporary file behind.
// The temporary is what makes the write atomic, and one left per failed attempt is a file that grows
// stale beside the real cursor and confuses whoever next looks at the directory.
func TestWriteCursorCleansUpWhenTheRenameFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A non-empty directory where the cursor file belongs: the temporary write succeeds and the
	// rename onto it cannot.
	path := filepath.Join(dir, "cursor.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := writeCursor(path, 5); err == nil {
		t.Fatal("writeCursor() over a non-empty directory = nil error")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the temporary file survived the failed rename, stat error = %v", err)
	}
}

// TestForwarderSleepRunsToItsTimer pins the other half of the wait: with no stop signal it waits the
// duration out and reports that the tail should keep going. A sleep that returned false on a plain
// timeout would stop the forwarder the first time it caught up.
func TestForwarderSleepRunsToItsTimer(t *testing.T) {
	t.Parallel()
	f := NewForwarder(seedAudits(t, 1), []Sink{&captureSink{}},
		filepath.Join(t.TempDir(), "cursor.json"), time.Second, nil)
	defer f.Close()
	start := time.Now()
	if !f.sleep(20 * time.Millisecond) {
		t.Fatal("sleep() reported stop on a plain timeout, so the tail exits when it catches up")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("sleep() returned after %v, want it to wait the duration out", elapsed)
	}
}

// TestForwarderStopsWhileBackingOff proves a shutdown during the failure backoff returns promptly
// rather than waiting out the delay. The backoff climbs to a minute against a collector that stays
// down, so a Close that had to wait for it would hold every restart for up to that long.
func TestForwarderStopsWhileBackingOff(t *testing.T) {
	t.Parallel()
	buf := &logBuffer{}
	f := NewForwarder(seedAudits(t, 2), []Sink{&captureSink{refuse: true}},
		filepath.Join(t.TempDir(), "cursor.json"), time.Hour, captureLogger(buf))
	if err := f.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "forward:") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "forward:") {
		t.Fatalf("no delivery failure was logged, got:\n%s", buf.String())
	}

	stopped := make(chan struct{})
	go func() {
		f.Close()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() waited out the backoff instead of stopping the tail")
	}
}
