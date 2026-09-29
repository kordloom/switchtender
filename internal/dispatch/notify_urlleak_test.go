package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// logBuffer collects encoded log lines so a test can assert on the exact text a delivery failure
// emitted, fields included. It is written by the dispatcher's notify goroutines and read by the
// test, so every access takes the lock.
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
// bytes that would reach the operator's log rather than a summary of them.
func captureLogger(buf *logBuffer) *zap.Logger {
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	return zap.New(zapcore.NewCore(enc, zapcore.AddSync(buf), zapcore.DebugLevel))
}

// deadAddr returns a host:port nothing listens on, so a delivery to it fails at once instead of
// hanging on a name lookup.
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

// TestNotifyFailureDoesNotLogSecretURL proves a failed notification delivery keeps the credential
// out of the log. A webhook, Slack, Discord, or Teams URL is itself the bearer secret, and a Twilio
// endpoint carries the account SID in its path, so neither the logged url field nor the transport
// error's own text may carry it. The error is the harder half: net/http wraps a dial failure in a
// *url.Error that re-embeds the whole address in its message, so masking the field alone leaves the
// secret in the line beside it.
func TestNotifyFailureDoesNotLogSecretURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the channel under test.
		Name string
		// Secret is the value that must appear in no log line and no stored run.
		Secret string
		// Options wires the channel at the dead address, carrying Secret.
		Options func(dead, secret string) []Option
		// Setup points a channel with a fixed base URL at the dead address.
		Setup func(d *Dispatcher, dead string)
	}{{ // Test 0: A server-wide webhook whose path is the credential.
		Name:   "webhook",
		Secret: "T0000-B0000-SUPERSECRETHOOK",
		Options: func(dead, secret string) []Option {
			return []Option{WithWebhooks([]string{"http://" + dead + "/services/" + secret})}
		},
	}, { // Test 1: A Twilio account whose SID is in the endpoint path.
		Name:   "twilio",
		Secret: "ACSUPERSECRETACCOUNTSID",
		Options: func(_, secret string) []Option {
			return []Option{WithTwilio(secret, "tok", "+15550000", []string{"+15551111"})}
		},
		Setup: func(d *Dispatcher, dead string) { d.twilioBaseURL = "http://" + dead },
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dead := deadAddr(t)
			buf := &logBuffer{}
			store := run.NewMemStore()
			runner := roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{ExitCode: 2}, nil
				})
			d := New(store, runner, captureLogger(buf), test.Options(dead, test.Secret)...)
			if test.Setup != nil {
				test.Setup(d, dead)
			}
			defer d.Close()

			created, err := d.Submit(context.Background(), "play.yml", "inv")
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if got := waitTerminal(t, store, created.ID); got.Status != run.StatusFailed {
				t.Fatalf("status = %q, want failed", got.Status)
			}
			// Close joins the notify goroutines, so every delivery attempt has reported by the
			// time the buffer is read.
			d.Close()

			logged := buf.String()
			// The delivery must actually have failed and been reported, or there is nothing to
			// assert on and a passing test would prove nothing.
			if !strings.Contains(logged, "dispatch: webhook:") {
				t.Fatalf("no delivery failure was logged, got:\n%s", logged)
			}
			if !strings.Contains(logged, dead) {
				t.Errorf("log names no endpoint host, want %q, got:\n%s", dead, logged)
			}
			if strings.Contains(logged, test.Secret) {
				t.Errorf("log leaked the channel secret %q:\n%s", test.Secret, logged)
			}

			stored, err := store.Get(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			body, err := json.Marshal(stored)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if strings.Contains(string(body), test.Secret) {
				t.Errorf("stored run leaked the channel secret %q: %s", test.Secret, body)
			}
		})
	}
}
