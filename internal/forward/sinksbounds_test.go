package forward

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestHTTPSinkNameShapes pins what a sink calls itself, which is the string the forwarder writes
// into the operator's log on every delivery failure. A Splunk HEC address carries its token in the
// path and a webhook address is a credential end to end, so the name may carry a scheme and a host
// and nothing else. A value that reveals no safe host at all falls back to a fixed label rather
// than printing whatever it holds.
func TestHTTPSinkNameShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the configured endpoint.
		In string
		// WantResult is the name a log line may show.
		WantResult string
	}{{ // Test 0: The Splunk HEC shape, token in the path.
		In:         "https://splunk.example:8088/services/collector/raw?token=SECRET",
		WantResult: "http https://splunk.example:8088/…",
	}, { // Test 1: An empty endpoint names nothing, so the sink gets a generic label.
		In: "", WantResult: "http sink",
	}, { // Test 2: An unparseable endpoint is redacted whole, so the label stands in for it.
		In: "http://%zz/path", WantResult: "http sink",
	}, { // Test 3: A value naming no host gets the label too, rather than leaking its opaque part.
		In: "mailto:oncall@example.com", WantResult: "http sink",
	}, { // Test 4: Userinfo is not the host, so a password in the endpoint never reaches a log.
		In: "https://user:hunter2@collector.example/in", WantResult: "http https://collector.example/…",
	}, { // Test 5: A bare host with no path still reads as a redaction.
		In: "https://collector.example", WantResult: "http https://collector.example/…",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := NewHTTPSink(test.In, nil, nil).Name()
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("Name() mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(got, "SECRET") || strings.Contains(got, "hunter2") {
				t.Errorf("Name() = %q, which is logged on every delivery failure", got)
			}
		})
	}
}

// TestHTTPSinkTreatsOnlyTwoHundredsAsDelivered pins the boundary between an accepted batch and a
// refused one. The forwarder advances its durable cursor when Deliver returns nil, so an answer
// read as success when the collector refused it is the one way an event is dropped and never
// redelivered.
func TestHTTPSinkTreatsOnlyTwoHundredsAsDelivered(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Status is what the collector answers.
		Status int
		// WantDelivered is whether the batch counts as accepted.
		WantDelivered bool
	}{
		{Status: 200, WantDelivered: true},  // Test 0: The bottom of the accepted range.
		{Status: 202, WantDelivered: true},  // Test 1: Accepted for later processing still counts.
		{Status: 204, WantDelivered: true},  // Test 2: No content is a normal collector answer.
		{Status: 299, WantDelivered: true},  // Test 3: The top of the range.
		{Status: 300, WantDelivered: false}, // Test 4: One above it.
		{Status: 301, WantDelivered: false}, // Test 5: A moved collector is not a delivery.
		{Status: 400, WantDelivered: false}, // Test 6: A rejected body did not arrive.
		{Status: 401, WantDelivered: false}, // Test 7: A bad token is a failure, not a delivery.
		{Status: 429, WantDelivered: false}, // Test 8: Rate limited means try again.
		{Status: 500, WantDelivered: false}, // Test 9: So does a collector fault.
		{Status: 503, WantDelivered: false}, // Test 10: And an outage.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.Status)
			}))
			defer srv.Close()
			sink := NewHTTPSink(srv.URL, nil, srv.Client())
			err := sink.Deliver(context.Background(), []Event{{ID: "a1", Seq: 1, Receipt: "1:aa"}})
			if delivered := err == nil; delivered != test.WantDelivered {
				t.Errorf("Deliver() against %d returned %v, delivered = %v, want %v",
					test.Status, err, delivered, test.WantDelivered)
			}
		})
	}
}

// TestHTTPSinkRefusesAnUnusableEndpoint proves a request that cannot even be built is a failure the
// forwarder retries rather than a batch quietly counted as delivered, and that the endpoint is
// masked out of the message on that path too. The request-building error carries the raw address
// just as a transport error does.
func TestHTTPSinkRefusesAnUnusableEndpoint(t *testing.T) {
	t.Parallel()
	sink := NewHTTPSink("http://%zz/services/collector/"+sinkSecret, nil, nil)
	err := sink.Deliver(context.Background(), []Event{{ID: "a1", Seq: 1}})
	if err == nil {
		t.Fatal("Deliver() to an unusable endpoint = nil, which advances the cursor over the batch")
	}
	if strings.Contains(err.Error(), sinkSecret) {
		t.Errorf("Deliver() error leaked the endpoint token: %v", err)
	}
}

// TestHTTPSinkRefusesACanceledContext proves a shutdown mid-delivery is reported as a failure, so
// the batch is redelivered by the next process rather than skipped by this one.
func TestHTTPSinkRefusesACanceledContext(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := NewHTTPSink(srv.URL, nil, srv.Client()).Deliver(ctx, []Event{{ID: "a1", Seq: 1}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Deliver() with a canceled context = %v, want the cancellation reported", err)
	}
}

// TestHTTPSinkSendsEveryEventOnce walks the batch sizes the forwarder actually hands a sink, from
// none to more than one page, and checks the body is one JSON object per line with nothing dropped
// or duplicated. NDJSON is what Splunk, Elastic, and Vector ingest without an adapter, and a
// missing newline turns a batch into one unparsable blob at the collector.
func TestHTTPSinkSendsEveryEventOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Count is how many events the batch carries.
		Count int
	}{
		{Count: 0},   // Test 0: An empty batch still posts, and the body is empty.
		{Count: 1},   // Test 1: One event.
		{Count: 2},   // Test 2: Two, so the separator matters.
		{Count: 600}, // Test 3: More than one forwarder page.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var body bytes.Buffer
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				_, _ = body.ReadFrom(r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			events := make([]Event, test.Count)
			for i := range events {
				events[i] = Event{ID: fmt.Sprintf("aud_%d", i), Seq: int64(i + 1),
					Receipt: fmt.Sprintf("%d:aa", i+1), At: time.Unix(int64(i), 0).UTC()}
			}
			if err := NewHTTPSink(srv.URL, nil, srv.Client()).
				Deliver(context.Background(), events); err != nil {
				t.Fatalf("Deliver() error = %v", err)
			}

			mu.Lock()
			raw := body.String()
			mu.Unlock()
			var got []Event
			for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
				if line == "" {
					continue
				}
				var e Event
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatalf("line %q is not one JSON object: %v", line, err)
				}
				got = append(got, e)
			}
			if diff := cmp.Diff(events, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("delivered events mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// fakeConn is a net.Conn a test drives: it records what was written and can fail the write, which
// is how the syslog sink's connection recovery is exercised without a real collector going away.
type fakeConn struct {
	net.Conn
	// mu guards written and closed.
	mu sync.Mutex
	// written holds every byte the sink wrote.
	written bytes.Buffer
	// writeErr, when set, is returned instead of writing.
	writeErr error
	// closed counts Close calls, so a leaked connection is visible.
	closed int
	// deadline records the last write deadline the sink asked for.
	deadline time.Time
}

// Write records the bytes or fails, as the test asked.
func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.written.Write(p)
}

// Close counts the call.
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

// SetWriteDeadline records what the sink asked for.
func (c *fakeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

// text returns everything written so far.
func (c *fakeConn) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.String()
}

// TestSyslogSinkName pins the label the forwarder logs. A collector address is a host and port with
// no credential in it, so unlike the HTTP endpoint it is shown whole; an operator reading a failure
// needs to know which collector stopped answering.
func TestSyslogSinkName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the collector address.
		In string
		// WantResult is the label.
		WantResult string
	}{
		{In: "collector.example:514", WantResult: "syslog collector.example:514"}, // Test 0: Normal.
		{In: "", WantResult: "syslog "},                                           // Test 1: Unset.
		{In: "[2001:db8::1]:6514", WantResult: "syslog [2001:db8::1]:6514"},       // Test 2: IPv6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := NewSyslogSink(test.In, false, "host").Name()
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("Name() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSyslogSinkFraming pins the exact bytes one event becomes, because octet counting is what lets
// a JSON body containing newlines survive the transport intact. A wrong length prefix
// desynchronizes the collector for every message after it, so the frame is checked by parsing it
// back rather than by eyeballing a prefix.
func TestSyslogSinkFraming(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Hostname is what the sink calls this sender.
		Hostname string
		// Events are the batch.
		Events []Event
		// WantHosts is the hostname field expected in each rendered header.
		WantHosts string
	}{{ // Test 0: The ordinary case, two events in one batch.
		Name: "two events", Hostname: "controller-1", WantHosts: "controller-1",
		Events: []Event{{ID: "aud_1", Seq: 41, Receipt: "41:aa", Actor: "op"},
			{ID: "aud_2", Seq: 42, Receipt: "42:bb", Actor: "op"}},
	}, { // Test 1: No hostname configured renders the RFC 5424 nil value rather than an empty field,
		// which would leave the header one token short and unparsable.
		Name: "no hostname", Hostname: "", WantHosts: "-",
		Events: []Event{{ID: "aud_1", Seq: 1, Receipt: "1:aa"}},
	}, { // Test 2: A path holding a newline and a quote still arrives as one framed message.
		Name: "newline in payload", Hostname: "h", WantHosts: "h",
		Events: []Event{{ID: "aud_1", Seq: 7, Receipt: "7:aa", Path: "/runs/a\nb\"c"}},
	}, { // Test 3: An empty batch writes nothing at all.
		Name: "empty batch", Hostname: "h", WantHosts: "h", Events: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			conn := &fakeConn{}
			sink := NewSyslogSink("collector:514", false, test.Hostname)
			sink.dial = func(context.Context) (net.Conn, error) { return conn, nil }
			sink.now = func() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) }
			if err := sink.Deliver(context.Background(), test.Events); err != nil {
				t.Fatalf("Deliver() error = %v", err)
			}
			msgs := unframe(t, conn.text())
			if len(msgs) != len(test.Events) {
				t.Fatalf("unframed %d messages, want %d from %q", len(msgs), len(test.Events),
					conn.text())
			}
			for i, msg := range msgs {
				prefix := fmt.Sprintf("<133>1 2026-08-04T12:00:00Z %s switchtender - seq-%d - ",
					test.WantHosts, test.Events[i].Seq)
				if !strings.HasPrefix(msg, prefix) {
					t.Fatalf("message %d = %q, want the RFC 5424 header %q", i, msg, prefix)
				}
				var back Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(msg, prefix)), &back); err != nil {
					t.Fatalf("message %d payload is not the JSON event: %v", i, err)
				}
				if diff := cmp.Diff(test.Events[i], back); diff != "" {
					t.Errorf("message %d round trip mismatch (-want +got):\n%s", i, diff)
				}
			}
		})
	}
}

// unframe splits octet counted syslog messages back into the messages they carry, failing the test
// on a length prefix that does not match what follows.
func unframe(t *testing.T, framed string) []string {
	t.Helper()
	var out []string
	for len(framed) > 0 {
		space := strings.IndexByte(framed, ' ')
		if space < 0 {
			t.Fatalf("frame %q carries no length prefix", framed)
		}
		var n int
		if _, err := fmt.Sscanf(framed[:space], "%d", &n); err != nil {
			t.Fatalf("frame length %q is not a number: %v", framed[:space], err)
		}
		framed = framed[space+1:]
		if n > len(framed) {
			t.Fatalf("frame claims %d bytes but only %d follow", n, len(framed))
		}
		out = append(out, framed[:n])
		framed = framed[n:]
	}
	return out
}

// TestSyslogSinkDropsTheConnectionOnAWriteFailure proves a failed write leaves no half-used
// connection behind. The forwarder's cursor makes the redelivery safe, but only if the next attempt
// redials: writing the rest of a batch onto a socket the collector has gone away from would report
// success for messages nothing received.
func TestSyslogSinkDropsTheConnectionOnAWriteFailure(t *testing.T) {
	t.Parallel()
	broken := &fakeConn{writeErr: errors.New("broken pipe")}
	healthy := &fakeConn{}
	var dials int
	sink := NewSyslogSink("collector:514", false, "h")
	sink.dial = func(context.Context) (net.Conn, error) {
		dials++
		if dials == 1 {
			return broken, nil
		}
		return healthy, nil
	}
	events := []Event{{ID: "aud_1", Seq: 1, Receipt: "1:aa"}}

	if err := sink.Deliver(context.Background(), events); err == nil {
		t.Fatal("Deliver() over a broken connection = nil, so the batch is counted as delivered")
	}
	if sink.conn != nil {
		t.Error("the failed connection was kept, so the next batch is written into a dead socket")
	}
	if broken.closed != 1 {
		t.Errorf("the failed connection was closed %d times, want exactly one", broken.closed)
	}
	if err := sink.Deliver(context.Background(), events); err != nil {
		t.Fatalf("the retry did not redial: %v", err)
	}
	if dials != 2 || healthy.text() == "" {
		t.Errorf("dials = %d and the second connection carries %q, want a redial that delivered",
			dials, healthy.text())
	}
}

// TestSyslogSinkReportsADialFailure proves an unreachable collector is a failure the forwarder
// retries rather than a batch silently accepted, and that no connection is recorded from the
// attempt.
func TestSyslogSinkReportsADialFailure(t *testing.T) {
	t.Parallel()
	sink := NewSyslogSink(deadAddr(t), false, "h")
	if err := sink.Deliver(context.Background(), []Event{{ID: "a", Seq: 1}}); err == nil {
		t.Fatal("Deliver() to a dead collector = nil, which advances the cursor over the batch")
	}
	if sink.conn != nil {
		t.Error("a failed dial left a connection on the sink")
	}
	if err := sink.Close(); err != nil {
		t.Errorf("Close() after a failed dial = %v, want nil", err)
	}
}

// TestSyslogSinkDeadlines pins that the write deadline comes from the caller's context when it has
// one and from a fixed ceiling when it does not. A forwarder wedged on a write to a collector that
// accepted the connection and then stopped reading is a forwarder that stopped, and the deadline
// reads the real clock even when the message clock is replaced, since a rendered timestamp is
// content and a deadline is transport.
func TestSyslogSinkDeadlines(t *testing.T) {
	t.Parallel()
	frozen := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("test 0", func(t *testing.T) { // Test 0: The context's deadline is used as it stands.
		t.Parallel()
		conn := &fakeConn{}
		sink := NewSyslogSink("collector:514", false, "h")
		sink.dial = func(context.Context) (net.Conn, error) { return conn, nil }
		sink.now = func() time.Time { return frozen }
		want := time.Now().Add(2 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), want)
		defer cancel()
		if err := sink.Deliver(ctx, []Event{{ID: "a", Seq: 1}}); err != nil {
			t.Fatalf("Deliver() error = %v", err)
		}
		if !conn.deadline.Equal(want) {
			t.Errorf("write deadline = %v, want the context's %v", conn.deadline, want)
		}
	})

	t.Run("test 1", func(t *testing.T) { // Test 1: With no deadline a real-clock ceiling is set.
		t.Parallel()
		conn := &fakeConn{}
		sink := NewSyslogSink("collector:514", false, "h")
		sink.dial = func(context.Context) (net.Conn, error) { return conn, nil }
		sink.now = func() time.Time { return frozen }
		before := time.Now()
		if err := sink.Deliver(context.Background(), []Event{{ID: "a", Seq: 1}}); err != nil {
			t.Fatalf("Deliver() error = %v", err)
		}
		if conn.deadline.Before(before) || conn.deadline.After(before.Add(time.Minute)) {
			t.Errorf("write deadline = %v, want roughly thirty seconds from the real clock, not "+
				"the replaced message clock at %v", conn.deadline, frozen)
		}
	})
}

// TestSyslogSinkCloseIsIdempotent proves closing twice does not double close the socket or report a
// failure. The forwarder closes every sink on shutdown, and a sink may already have dropped its
// connection after a write error.
func TestSyslogSinkCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{}
	sink := NewSyslogSink("collector:514", false, "h")
	sink.dial = func(context.Context) (net.Conn, error) { return conn, nil }
	if err := sink.Deliver(context.Background(), []Event{{ID: "a", Seq: 1}}); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if conn.closed != 1 {
		t.Errorf("the connection was closed %d times, want exactly one", conn.closed)
	}
}

// TestSyslogSinkOverTLSRefusesAnUntrustedCollector is the fail-closed check on the encrypted
// transport. Audit events name who changed what on which host, so a TLS sink that accepted any
// certificate would ship the whole trail to whatever answered on the address, and the operator who
// chose TLS would have no way to tell. The sink must refuse rather than deliver.
func TestSyslogSinkOverTLSRefusesAnUntrustedCollector(t *testing.T) {
	t.Parallel()
	ln := selfSignedListener(t)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Drive the handshake so the client learns the certificate is untrusted.
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()

	sink := NewSyslogSink(ln.Addr().String(), true, "controller-1")
	err := sink.Deliver(context.Background(), []Event{{ID: "aud_1", Seq: 1, Receipt: "1:aa"}})
	if err == nil {
		t.Fatal("Deliver() over TLS to an untrusted collector = nil, so the audit trail was shipped " +
			"to whatever answered the address")
	}
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if !errors.As(err, &unknown) && !errors.As(err, &hostname) {
		t.Errorf("Deliver() error = %v, want the certificate refused", err)
	}
	if sink.conn != nil {
		t.Error("a refused TLS handshake left a connection on the sink")
	}
}

// selfSignedListener returns a TLS listener holding a certificate no client trusts.
func selfSignedListener(t *testing.T) net.Listener {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "untrusted collector"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Listen() error = %v", err)
	}
	return ln
}

// TestMsgID pins the collector's sort and deduplication key. It is the chain sequence, so a
// collector orders and deduplicates on it without parsing the body, which is the whole reason the
// forwarder can promise at least once rather than exactly once.
func TestMsgID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the event's chain sequence.
		In int64
		// WantResult is the MSGID.
		WantResult string
	}{
		{In: 0, WantResult: "seq-0"},                               // Test 0: Unset.
		{In: 1, WantResult: "seq-1"},                               // Test 1: The first entry.
		{In: 9007199254740993, WantResult: "seq-9007199254740993"}, // Test 2: Past float range.
		{In: -1, WantResult: "seq--1"},                             // Test 3: Never produced.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, msgID(Event{Seq: test.In})); diff != "" {
				t.Errorf("msgID() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
