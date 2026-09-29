package forward

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// sinkSecret is the token an HTTP sink URL carries in its path, the reason Name reports scheme and
// host only. A Splunk HEC address ends in exactly this shape.
const sinkSecret = "HECSUPERSECRETTOKEN"

// logBuffer collects encoded log lines so a test can assert on the exact text the forwarder emitted.
// The tail goroutine writes it while the test reads, so every access takes the lock.
type logBuffer struct {
	// mu guards buf.
	mu sync.Mutex
	// buf holds the encoded lines.
	buf bytes.Buffer
}

// Write appends one encoded log line.
func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// Sync satisfies zapcore.WriteSyncer and does nothing.
func (l *logBuffer) Sync() error { return nil }

// String returns everything logged so far.
func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLogger returns a logger encoding every field into buf as JSON, so an assertion sees the
// bytes that would reach the operator's log.
func captureLogger(buf *logBuffer) *zap.Logger {
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	return zap.New(zapcore.NewCore(enc, zapcore.AddSync(buf), zapcore.DebugLevel))
}

// deadAddr returns a host:port nothing listens on, so a delivery to it fails at once.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return addr
}

// TestHTTPSinkDeliverErrorHidesEndpoint proves a transport failure returns an error naming the sink
// by scheme and host only. Name exists to keep the token a Splunk HEC path carries out of the log,
// and net/http hands back a *url.Error that re-embeds the whole address, so the error the sink
// returns must be unwrapped and masked or the redaction Name provides is undone by the error beside
// it.
func TestHTTPSinkDeliverErrorHidesEndpoint(t *testing.T) {
	t.Parallel()
	dead := deadAddr(t)
	sink := NewHTTPSink("http://"+dead+"/services/collector/"+sinkSecret, nil, nil)

	err := sink.Deliver(context.Background(), []Event{{ID: "a1", Seq: 1, Receipt: "1:aa"}})
	if err == nil {
		t.Fatal("Deliver() error = nil, want a transport failure")
	}
	if strings.Contains(err.Error(), sinkSecret) {
		t.Errorf("Deliver() error leaked the endpoint token: %v", err)
	}
	if !strings.Contains(err.Error(), dead) {
		t.Errorf("Deliver() error names no host, want %q, got %v", dead, err)
	}
	// The cause must survive the masking, or a caller can no longer tell a refused connection from
	// a rejected batch.
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("Deliver() error = %v, want the dial failure still in the chain", err)
	}
}

// TestForwarderLogsNoSinkEndpoint proves the token an HTTP sink URL carries reaches no log line when
// the collector is unreachable. The forwarder logs every delivery failure, so this is the line the
// leak would show up in.
func TestForwarderLogsNoSinkEndpoint(t *testing.T) {
	t.Parallel()
	dead := deadAddr(t)
	buf := &logBuffer{}
	sink := NewHTTPSink("http://"+dead+"/services/collector/"+sinkSecret, nil, nil)
	f := NewForwarder(seedAudits(t, 3), []Sink{sink},
		filepath.Join(t.TempDir(), "cursor"), time.Second, captureLogger(buf))
	if err := f.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "forward:") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	f.Close()

	logged := buf.String()
	if !strings.Contains(logged, "forward:") {
		t.Fatalf("no delivery failure was logged, got:\n%s", logged)
	}
	if strings.Contains(logged, sinkSecret) {
		t.Errorf("log leaked the sink endpoint token %q:\n%s", sinkSecret, logged)
	}
	if !strings.Contains(logged, dead) {
		t.Errorf("log names no sink host, want %q, got:\n%s", dead, logged)
	}
}
