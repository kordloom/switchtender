package forward

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// seedAudits appends n entries and returns the store.
func seedAudits(t *testing.T, n int) audit.Store {
	t.Helper()
	audits := audit.NewMemStore()
	for i := 0; i < n; i++ {
		e := &audit.Entry{ID: audit.NewID(), At: time.Now().UTC(), Actor: "op",
			Method: "POST", Path: "/v1/runs"}
		if err := audits.Append(context.Background(), e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return audits
}

// captureSink records every batch it accepts, refusing while refuse is set.
type captureSink struct {
	// mu guards batches and refuse.
	mu sync.Mutex
	// batches holds every accepted delivery.
	batches [][]Event
	// refuse makes Deliver fail without recording.
	refuse bool
}

// Deliver records the batch, or refuses.
func (c *captureSink) Deliver(_ context.Context, events []Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse {
		return fmt.Errorf("the collector is down")
	}
	batch := make([]Event, len(events))
	copy(batch, events)
	c.batches = append(c.batches, batch)
	return nil
}

// Name names the sink.
func (c *captureSink) Name() string { return "capture" }

// Close does nothing.
func (c *captureSink) Close() error { return nil }

// all returns every accepted event in order.
func (c *captureSink) all() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for _, b := range c.batches {
		out = append(out, b...)
	}
	return out
}

func TestForwarderDeliversTheChainWithReceipts(t *testing.T) {
	t.Parallel()
	audits := seedAudits(t, 3)
	sink := &captureSink{}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(audits, []Sink{sink}, cursor, time.Second, nil)

	if n, err := f.forwardOnce(context.Background()); err != nil || n != 3 {
		t.Fatalf("forwardOnce() = %d, %v, want the three entries", n, err)
	}
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	got := sink.all()
	for i, e := range got {
		if e.Receipt != audit.Receipt(chain[i]) {
			t.Errorf("event %d receipt = %q, want the chain's %q", i, e.Receipt, audit.Receipt(chain[i]))
		}
	}
	if got[2].Actor != "op" || got[2].Seq != chain[2].Seq {
		t.Errorf("event = %+v, want the entry's fields carried", got[2])
	}

	// Caught up: nothing is redelivered.
	if n, err := f.forwardOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("caught-up forwardOnce() = %d, %v, want nothing", n, err)
	}

	// The chain grows; only the new entries move.
	e := &audit.Entry{ID: audit.NewID(), At: time.Now().UTC(), Actor: "op2",
		Method: "POST", Path: "/v1/runs"}
	if err := audits.Append(context.Background(), e); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if n, err := f.forwardOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("incremental forwardOnce() = %d, %v, want the one new entry", n, err)
	}
	if got := sink.all(); len(got) != 4 || got[3].Actor != "op2" {
		t.Fatalf("delivered = %d events ending %+v, want the new entry once", len(got), got[len(got)-1])
	}
}

func TestForwarderHoldsTheCursorUntilEverySinkAccepts(t *testing.T) {
	t.Parallel()
	audits := seedAudits(t, 2)
	good, bad := &captureSink{}, &captureSink{refuse: true}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(audits, []Sink{good, bad}, cursor, time.Second, nil)

	if _, err := f.forwardOnce(context.Background()); err == nil {
		t.Fatal("forwardOnce() with a refusing sink = nil error, want the failure surfaced")
	}
	if seq, err := readCursor(cursor); err != nil || seq != 0 {
		t.Fatalf("cursor after refusal = %d, %v, want unmoved at 0", seq, err)
	}

	// The collector recovers: the batch is redelivered whole. The sink that already accepted
	// sees it twice; at least once is the contract and the receipt is the deduplication key.
	bad.mu.Lock()
	bad.refuse = false
	bad.mu.Unlock()
	if n, err := f.forwardOnce(context.Background()); err != nil || n != 2 {
		t.Fatalf("recovered forwardOnce() = %d, %v, want the batch through", n, err)
	}
	if len(good.all()) != 4 || len(bad.all()) != 2 {
		t.Errorf("deliveries = %d good, %d bad; want the accepted sink redelivered and the "+
			"recovered one caught up", len(good.all()), len(bad.all()))
	}
	if seq, _ := readCursor(cursor); seq != 2 {
		t.Errorf("cursor = %d, want advanced past the delivered batch", seq)
	}
}

func TestForwarderPagesALongChain(t *testing.T) {
	t.Parallel()
	audits := seedAudits(t, 5)
	sink := &captureSink{}
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	f := NewForwarder(audits, []Sink{sink}, cursor, time.Second, nil)
	f.batch = 2

	var sizes []int
	for rounds := 0; ; rounds++ {
		if rounds > 10 {
			t.Fatal("forwarder never caught up; the cursor is not advancing")
		}
		n, err := f.forwardOnce(context.Background())
		if err != nil {
			t.Fatalf("forwardOnce() error = %v", err)
		}
		if n == 0 {
			break
		}
		sizes = append(sizes, n)
	}
	if fmt.Sprint(sizes) != "[2 2 1]" {
		t.Errorf("batch sizes = %v, want the chain paged as [2 2 1]", sizes)
	}
	if got := sink.all(); len(got) != 5 || got[4].Seq <= got[0].Seq {
		t.Errorf("delivered %d events, want all five in chain order", len(got))
	}
}

func TestForwarderResumesAcrossRestart(t *testing.T) {
	t.Parallel()
	audits := seedAudits(t, 3)
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	first := &captureSink{}
	f := NewForwarder(audits, []Sink{first}, cursor, time.Second, nil)
	if _, err := f.forwardOnce(context.Background()); err != nil {
		t.Fatalf("forwardOnce() error = %v", err)
	}

	// A new process over the same cursor forwards only what came after.
	e := &audit.Entry{ID: audit.NewID(), At: time.Now().UTC(), Actor: "later",
		Method: "POST", Path: "/v1/runs"}
	if err := audits.Append(context.Background(), e); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	second := &captureSink{}
	restarted := NewForwarder(audits, []Sink{second}, cursor, time.Second, nil)
	if n, err := restarted.forwardOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("restarted forwardOnce() = %d, %v, want only the new entry", n, err)
	}
	if got := second.all(); len(got) != 1 || got[0].Actor != "later" {
		t.Errorf("restarted delivery = %+v, want the one entry appended after the cursor", got)
	}
}

func TestCursorRefusesCorruption(t *testing.T) {
	t.Parallel()
	cursor := filepath.Join(t.TempDir(), "cursor.json")
	if err := os.WriteFile(cursor, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := readCursor(cursor); err == nil {
		t.Fatal("readCursor() on garbage = nil error; restreaming a whole chain into a SIEM " +
			"because of a corrupt byte is the failure mode this guard exists for")
	}
	f := NewForwarder(seedAudits(t, 1), []Sink{&captureSink{}}, cursor, time.Second, nil)
	defer f.Close()
	if err := f.Start(); err == nil {
		t.Fatal("Start() over a corrupt cursor = nil error, want the startup refused loudly")
	}
}

func TestHTTPSinkSpeaksNDJSONAndRefusesRefusal(t *testing.T) {
	t.Parallel()
	var gotBody string
	var gotType, gotAuth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotBody, gotType, gotAuth = string(body), r.Header.Get("Content-Type"),
			r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	sink := NewHTTPSink(srv.URL, map[string]string{"Authorization": "Splunk tok"}, srv.Client())

	events := []Event{
		{ID: "aud_1", Seq: 1, Receipt: "1:aa", Actor: "op", At: time.Now().UTC()},
		{ID: "aud_2", Seq: 2, Receipt: "2:bb", Actor: "op", At: time.Now().UTC()},
	}
	if err := sink.Deliver(context.Background(), events); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if gotType != "application/x-ndjson" || gotAuth != "Splunk tok" {
		t.Errorf("headers = %q, %q, want ndjson with the auth header", gotType, gotAuth)
	}
	lines := strings.Split(strings.TrimSpace(gotBody), "\n")
	if len(lines) != 2 {
		t.Fatalf("body has %d lines, want one JSON object per event", len(lines))
	}
	var back Event
	if err := json.Unmarshal([]byte(lines[1]), &back); err != nil || back.Receipt != "2:bb" {
		t.Errorf("line 2 = %q (%v), want the second event with its receipt", lines[1], err)
	}

	status = http.StatusServiceUnavailable
	if err := sink.Deliver(context.Background(), events); err == nil {
		t.Error("Deliver() on a 503 = nil error; a refused delivery did not happen")
	}
}

func TestSyslogSinkFramesEveryEvent(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()
	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		var msgs []string
		for i := 0; i < 2; i++ {
			lenStr, err := r.ReadString(' ')
			if err != nil {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(lenStr))
			if err != nil {
				return
			}
			msg := make([]byte, n)
			if _, err := io.ReadFull(r, msg); err != nil {
				return
			}
			msgs = append(msgs, string(msg))
		}
		received <- strings.Join(msgs, "\x00")
	}()

	sink := NewSyslogSink(ln.Addr().String(), false, "controller-1")
	sink.now = func() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) }
	events := []Event{
		{ID: "aud_1", Seq: 41, Receipt: "41:aa", Actor: "op"},
		{ID: "aud_2", Seq: 42, Receipt: "42:bb", Actor: "op"},
	}
	if err := sink.Deliver(context.Background(), events); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	defer func() { _ = sink.Close() }()

	select {
	case joined := <-received:
		msgs := strings.Split(joined, "\x00")
		if !strings.HasPrefix(msgs[0], "<133>1 2026-08-04T12:00:00Z controller-1 switchtender - seq-41 - ") {
			t.Errorf("message = %q, want the RFC 5424 header with the seq MSGID", msgs[0])
		}
		var back Event
		payload := msgs[1][strings.Index(msgs[1], "- {")+2:]
		if err := json.Unmarshal([]byte(payload), &back); err != nil || back.Receipt != "42:bb" {
			t.Errorf("payload = %q (%v), want the JSON event with its receipt", payload, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collector never received the framed messages")
	}
}
