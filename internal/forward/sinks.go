package forward

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/kordloom/switchtender/internal/util"
)

// HTTPSink delivers events as one NDJSON body per batch: one JSON object per line, the shape
// Splunk HEC raw endpoints, Elastic pipelines, and log routers like Vector all ingest without an
// adapter in the way.
type HTTPSink struct {
	// url is the endpoint batches are posted to.
	url string
	// headers are set on every request, e.g. an Authorization token.
	headers map[string]string
	// client posts the batches.
	client *http.Client
}

// NewHTTPSink returns a sink posting to url with the given headers. A nil client gets a 30
// second timeout, since a forwarder wedged on one hung POST is a forwarder that stopped.
func NewHTTPSink(url string, headers map[string]string, client *http.Client) *HTTPSink {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPSink{url: url, headers: headers, client: client}
}

// Name identifies the sink in logs by scheme and host, the path and query replaced by the mask
// marker. The full URL is kept out because it is logged on every delivery failure, and a path or
// query can carry a token (a Splunk HEC URL often ends in the token) that has no business in a log
// line. It masks through the same helper the delivery error does, so the two cannot drift.
func (s *HTTPSink) Name() string {
	masked := util.MaskURL(s.url)
	if masked == "" || masked == util.MaskMarker {
		return "http sink"
	}
	return "http " + masked
}

// Deliver posts the batch and treats anything but a 2xx answer as failure, because a delivery
// the endpoint refused did not happen, whatever the transport says.
func (s *HTTPSink) Deliver(ctx context.Context, events []Event) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, &body)
	if err != nil {
		return util.MaskURLError(err, s.url)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	res, err := s.client.Do(req)
	if err != nil {
		// The client hands back a *url.Error carrying the whole endpoint, which the forwarder logs
		// on every failure. That undoes the redaction Name exists to provide, so the address is
		// masked out of the message before the error leaves the sink. The cause stays in the chain.
		return util.MaskURLError(err, s.url)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("answered %s", res.Status)
	}
	return nil
}

// Close releases nothing; the HTTP client owns no connection worth holding.
func (s *HTTPSink) Close() error { return nil }

// SyslogSink delivers events over TCP syslog, one RFC 5424 message per event with the JSON event
// as the message body, framed by octet counting (RFC 6587) so JSON survives the transport intact.
type SyslogSink struct {
	// addr is the collector's host:port.
	addr string
	// useTLS wraps the connection in TLS.
	useTLS bool
	// hostname names this sender in the syslog header.
	hostname string
	// conn is the open connection, nil until the first delivery or after an error.
	conn net.Conn
	// dial opens the connection, replaced in tests.
	dial func(ctx context.Context) (net.Conn, error)
	// now reads the clock for message timestamps, replaced in tests.
	now func() time.Time
}

// NewSyslogSink returns a sink for the collector at addr.
func NewSyslogSink(addr string, useTLS bool, hostname string) *SyslogSink {
	s := &SyslogSink{addr: addr, useTLS: useTLS, hostname: hostname, now: time.Now}
	s.dial = func(ctx context.Context) (net.Conn, error) {
		d := &net.Dialer{Timeout: 15 * time.Second}
		if s.useTLS {
			return (&tls.Dialer{NetDialer: d}).DialContext(ctx, "tcp", s.addr)
		}
		return d.DialContext(ctx, "tcp", s.addr)
	}
	return s
}

// Name identifies the sink in logs.
func (s *SyslogSink) Name() string { return "syslog " + s.addr }

// Deliver writes every event as one framed message. Any write error drops the connection so the
// next attempt redials; the forwarder's cursor makes the redelivery safe.
func (s *SyslogSink) Deliver(ctx context.Context, events []Event) error {
	if s.conn == nil {
		conn, err := s.dial(ctx)
		if err != nil {
			return err
		}
		s.conn = conn
	}
	// The deadline reads the real clock even when the message clock is replaced: a rendered
	// timestamp is content, a deadline is transport.
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.conn.SetWriteDeadline(deadline)
	} else {
		_ = s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	var frame bytes.Buffer
	for _, e := range events {
		msg, err := s.message(e)
		if err != nil {
			return err
		}
		fmt.Fprintf(&frame, "%d %s", len(msg), msg)
	}
	if _, err := s.conn.Write(frame.Bytes()); err != nil {
		_ = s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// message renders one RFC 5424 line: facility local0, severity notice, app switchtender, the
// entry id as MSGID, and the JSON event as the body.
func (s *SyslogSink) message(e Event) (string, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	host := s.hostname
	if host == "" {
		host = "-"
	}
	return fmt.Sprintf("<133>1 %s %s switchtender - %s - %s",
		s.now().UTC().Format(time.RFC3339), host, msgID(e), body), nil
}

// msgID is the syslog MSGID for an event: its chain sequence, so a collector sorts and
// deduplicates on it without parsing the body.
func msgID(e Event) string {
	return "seq-" + strconv.FormatInt(e.Seq, 10)
}

// Close drops the connection if one is open.
func (s *SyslogSink) Close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}
