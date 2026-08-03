package dispatch

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// TestStreamMaskerDrainWithholdsPartialSecret proves draining a quiet stream releases the safe prefix
// but keeps back a suffix that is the leading half of a secret, so the secret is redacted whole once
// its remainder arrives rather than leaking its first half. Before the fix drain flushed the whole
// tail, and the leading half reached the store and live viewers unredacted.
func TestStreamMaskerDrainWithholdsPartialSecret(t *testing.T) {
	t.Parallel()
	const secret = "TOPSECRETPASSWORD"
	m := &masker{}
	m.set([]string{secret})
	sm := &streamMasker{mask: m}

	// A write ending inside the secret is withheld whole by next.
	if out := sm.next([]byte("log: TOPSEC")); len(out) != 0 {
		t.Fatalf("next() released %q, want it all withheld", out)
	}
	// A quiet timer release emits the safe prefix and keeps the partial secret back.
	if got := string(sm.drain()); got != "log: " {
		t.Errorf("drain() = %q, want the safe prefix %q", got, "log: ")
	}
	// The remainder arrives; the secret is redacted whole, never in halves.
	got := string(sm.next([]byte("RETPASSWORD done"))) + string(sm.flush())
	if strings.Contains("log: "+got, "TOPSEC") {
		t.Errorf("drain leaked a partial secret across the release: %q then %q", "log: ", got)
	}
	if want := maskToken + " done"; got != want {
		t.Errorf("after drain, rest = %q, want %q", got, want)
	}
}

// TestStreamMaskerDrainReleasesOrdinaryOutput proves a quiet run's ordinary output, which begins no
// secret, is released whole rather than held back for the length of the longest secret, so a slow run
// does not show a blank log.
func TestStreamMaskerDrainReleasesOrdinaryOutput(t *testing.T) {
	t.Parallel()
	m := &masker{}
	// A long secret, as a private key credential produces.
	m.set([]string{strings.Repeat("k", 1600)})
	sm := &streamMasker{mask: m}

	if out := sm.next([]byte("starting up\n")); len(out) != 0 {
		t.Fatalf("next() released %q, want it withheld pending more output", out)
	}
	if got := string(sm.drain()); got != "starting up\n" {
		t.Errorf("drain() = %q, want the whole line, since it begins no secret", got)
	}
	if extra := sm.flush(); len(extra) != 0 {
		t.Errorf("flush() = %q, want nothing left after the drain", extra)
	}
}

// TestLogSinkReleaseHeldWithholdsPartialSecret drives the hold timer's release between the two writes
// that carry the halves of a secret and proves the secret never reaches the stored log unredacted.
// Before the fix releaseHeld flushed the leading half, so the plaintext secret survived the stream in
// two pieces.
func TestLogSinkReleaseHeldWithholdsPartialSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	r := &run.Run{ID: "run_partial", Status: run.StatusRunning}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	m := &masker{}
	m.set([]string{"TOPSECRETPASSWORD"})
	sink := &logSink{store: store, id: r.ID, log: zap.NewNop(), publisher: &capturingPublisher{}, mask: m}

	if _, err := sink.Write([]byte("log: TOPSEC")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// The hold timer fires between the two halves.
	sink.releaseHeld()
	if _, err := sink.Write([]byte("RETPASSWORD done")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	sink.flush()

	got, err := store.Log(ctx, r.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	for _, leak := range []string{"TOPSECRETPASSWORD", "TOPSECRET", "TOPSEC"} {
		if strings.Contains(string(got), leak) {
			t.Fatalf("stored log %q leaked %q despite the mid-stream release", got, leak)
		}
	}
	if want := "log: " + maskToken + " done"; string(got) != want {
		t.Errorf("stored log = %q, want %q", got, want)
	}
}

// orderProbe is a Publisher whose PublishLog appends without its own lock, so it relies on the log
// sink to serialize emission. If the sink emits from two goroutines at once, the append races and the
// detector flags it; with emission serialized under the sink's lock the appends land in order.
type orderProbe struct {
	// chunks holds every published chunk, in the order PublishLog was called.
	chunks [][]byte
}

// PublishEvents ignores events.
func (p *orderProbe) PublishEvents(string, []event.Event) {}

// PublishLog records a copy of the chunk without synchronizing, relying on the sink's ordering.
func (p *orderProbe) PublishLog(_ string, chunk []byte) {
	p.chunks = append(p.chunks, append([]byte(nil), chunk...))
}

// CloseRun ignores the close.
func (p *orderProbe) CloseRun(string) {}

// joined returns the published chunks concatenated in call order.
func (p *orderProbe) joined() []byte {
	var out []byte
	for _, c := range p.chunks {
		out = append(out, c...)
	}
	return out
}

// TestLogSinkEmitsInProductionOrderUnderRace runs the writer and the hold-timer release against one
// sink at once. Emission must be serialized in production order, so the bytes the publisher sees match
// the bytes the store holds, with none reordered or duplicated. Before the fix emit ran outside the
// sink's lock, so a write and a release could store or publish their chunks out of order; the race
// detector catches the overlap on the probe's unsynchronized append.
func TestLogSinkEmitsInProductionOrderUnderRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	r := &run.Run{ID: "run_order", Status: run.StatusRunning}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	m := &masker{}
	m.set([]string{"SECRET"})
	probe := &orderProbe{}
	sink := &logSink{store: store, id: r.ID, log: zap.NewNop(), publisher: probe, mask: m}

	const iterations = 2000
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < iterations; i++ {
			// Fprintf formats the line and calls sink.Write once with it, driving the write path.
			if _, err := fmt.Fprintf(sink, "line %04d plain output\n", i); err != nil {
				t.Errorf("Write() error = %v", err)
				return
			}
			runtime.Gosched()
		}
	}()
	// The releaser stands in for the hold timer, draining alongside the writer for its whole life so
	// their emits overlap rather than one finishing before the other starts.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				sink.releaseHeld()
				runtime.Gosched()
			}
		}
	}()
	wg.Wait()
	sink.flush()

	if len(probe.chunks) == 0 {
		t.Fatal("no output was emitted")
	}
	stored, err := store.Log(ctx, r.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	// Serialized emission stores and publishes the same bytes in the same order, so the two views are
	// identical. Reordering or a dropped chunk would break this equality.
	if diff := cmp.Diff(string(stored), string(probe.joined())); diff != "" {
		t.Errorf("stored and published streams differ (-store +publish):\n%s", diff)
	}
}
