package dispatch

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// smtpSink is a minimal SMTP server speaking just enough of RFC 5321 to receive one message, so
// the emailer is tested against a real socket exchange rather than a mock of our own assumptions.
// It records the envelope and the message body it was handed.
type smtpSink struct {
	// addr is the listener's host:port.
	addr string
	// got receives the full DATA payload once, then the envelope recipients.
	got chan string
	// rcpt receives each RCPT TO address.
	rcpt chan string
}

// startSMTPSink listens on a loopback port and serves exactly one SMTP session.
func startSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &smtpSink{addr: ln.Addr().String(), got: make(chan string, 1), rcpt: make(chan string, 8)}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		say := func(line string) { _, _ = fmt.Fprintf(conn, "%s\r\n", line) }
		say("220 sink ready")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					s.got <- data.String()
					say("250 queued")
					inData = false
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				// No STARTTLS on offer: loopback, and the emailer only upgrades when offered.
				say("250-sink")
				say("250 OK")
			case strings.HasPrefix(line, "MAIL FROM:"):
				say("250 OK")
			case strings.HasPrefix(line, "RCPT TO:"):
				s.rcpt <- strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<> ")
				say("250 OK")
			case line == "DATA":
				say("354 go")
				inData = true
			case line == "QUIT":
				say("221 bye")
				return
			default:
				say("250 OK")
			}
		}
	}()
	return s
}

// TestSMTPEmailerDeliversARealMessage holds the emailer to a live SMTP exchange: correct
// envelope, correct headers, and the body it was asked to send. It sat at zero coverage while
// being the transport a Team install's failure notifications ride, which is the one message an
// operator must be able to trust arrives shaped like mail.
func TestSMTPEmailerDeliversARealMessage(t *testing.T) {
	t.Parallel()
	sink := startSMTPSink(t)
	e := NewSMTPEmailer(sink.addr, "switchtender@example.com",
		[]string{"ops@example.com"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Send(ctx, "run failed", "run run_1 failed on web01"); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	select {
	case addr := <-sink.rcpt:
		if addr != "ops@example.com" {
			t.Errorf("RCPT TO %q, want the configured recipient", addr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sink never saw RCPT TO")
	}
	select {
	case msg := <-sink.got:
		for _, want := range []string{"From: switchtender@example.com", "To: ops@example.com",
			"Subject: run failed", "run run_1 failed on web01"} {
			if !strings.Contains(msg, want) {
				t.Errorf("delivered message is missing %q:\n%s", want, msg)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sink never received DATA")
	}
}
