package cmd

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/server"
)

// TestEnforcedAuthOptionActuallyEnforces proves the option external auth produces is the enforcing
// one, not merely a non-nil function.
//
// enforcedAuthOption returns a server.Option in both directions: a no-op when there is no external
// provider and server.WithEnforcedAuth when there is. Both are non-nil closures, so a test that only
// checks the result is non-nil passes just as happily when the enforcing branch is replaced by the
// no-op. That substitution is the worst defect this file can hold. Open mode is keyed on finding no
// tokens and no accounts, and an install whose way in is single sign-on has exactly that shape
// before anybody has signed in, so the whole API would be served to anonymous callers with admin
// authority while the flags say sign-on is required.
//
// The option is applied to a real server and the result compared against applying the genuine
// article, so the assertion is about what reaches the server rather than about the closure's type.
func TestEnforcedAuthOptionActuallyEnforces(t *testing.T) {
	t.Parallel()

	// Test 0: With external auth the option must do exactly what WithEnforcedAuth does.
	var got, want server.Server
	enforcedAuthOption(true)(&got)
	server.WithEnforcedAuth()(&want)
	if !reflect.DeepEqual(got, want) {
		t.Error("enforcedAuthOption(true) did not put the server in the state WithEnforcedAuth " +
			"does, so an install with single sign-on configured would still fall back to open " +
			"mode and serve the API to anonymous callers with admin authority")
	}

	// Test 1: Without external auth the option must leave the server exactly as it found it, so the
	// token count stays in charge of the posture.
	var untouched, unset server.Server
	enforcedAuthOption(false)(&unset)
	if !reflect.DeepEqual(unset, untouched) {
		t.Error("enforcedAuthOption(false) changed the server; with no external provider the " +
			"token count decides the posture and this option must be inert")
	}

	// Test 2: The two directions must be distinguishable. If they are not, the flag decides nothing.
	if reflect.DeepEqual(got, unset) {
		t.Error("enforcedAuthOption(true) and enforcedAuthOption(false) leave the server in the " +
			"same state, so configuring single sign-on changes nothing about enforcement")
	}
}

// TestBuildEmailerAuthenticatesWithTheConfiguredPassword proves the SMTP credential the notifier
// presents is the username from the flag and the password from the environment.
//
// The password is deliberately kept out of the flags so it never appears in help output or a process
// listing, which means the only thing tying it to the relay is this one line. A test that sets no
// username never reaches the branch at all, so a mix-up there, sending the username as the password
// or dropping the environment lookup, leaves every notification silently unsent: the relay rejects
// the credential, the run finishes, and nobody is told the run failed.
//
// A relay on loopback is stood up and the AUTH exchange captured, so the assertion is on the bytes
// that reach a server rather than on the shape of the value handed to the constructor. Go's PLAIN
// mechanism sends credentials unencrypted only to localhost, which is what this is.
func TestBuildEmailerAuthenticatesWithTheConfiguredPassword(t *testing.T) {
	// Not parallel: the SMTP flags are package globals.
	const username = "notifier@switchtender"
	const password = "s3cret-relay-password"

	addr, authLine := startCapturingRelay(t)

	setString(t, &smtpAddr, addr)
	setString(t, &smtpFrom, "switchtender@example")
	setString(t, &smtpUsername, username)
	setString(t, &notifyOn, "failure")
	t.Setenv("SWITCHTENDER_SMTP_PASSWORD", password)
	old := smtpTo
	t.Cleanup(func() { smtpTo = old })
	smtpTo = []string{"ops@example"}

	emailer, _ := buildEmailer()
	if emailer == nil {
		t.Fatal("buildEmailer() returned no emailer for a fully configured relay")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := emailer.Send(ctx, "subject", "body"); err != nil {
		t.Fatalf("Send() error = %v; the notifier could not complete the exchange", err)
	}

	var line string
	select {
	case line = <-authLine:
	case <-ctx.Done():
		t.Fatal("the relay never received an AUTH command, so the notifier sent no credential at " +
			"all and a relay that requires one would reject every notification")
	}

	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		t.Fatalf("AUTH PLAIN payload %q is not base64: %v", line, err)
	}
	// PLAIN is identity NUL username NUL password.
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		t.Fatalf("AUTH PLAIN payload has %d fields, want 3: %q", len(parts), raw)
	}
	if parts[1] != username {
		t.Errorf("AUTH PLAIN presented username %q, want %q", parts[1], username)
	}
	if parts[2] != password {
		t.Errorf("AUTH PLAIN presented password %q, want the value of "+
			"SWITCHTENDER_SMTP_PASSWORD; the relay rejects this credential and every "+
			"notification is silently lost", parts[2])
	}
	if parts[2] == username {
		t.Error("AUTH PLAIN sent the username as the password, so the environment password is " +
			"never used and the username is disclosed to the relay as a secret")
	}
}

// startCapturingRelay runs a minimal SMTP server on loopback that advertises AUTH PLAIN and accepts
// a whole message, and returns its address and a channel carrying the base64 payload of the AUTH
// command. It speaks only enough of the protocol for one delivery, which is all the notifier does.
func startCapturingRelay(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		write := func(s string) { _, _ = fmt.Fprintf(conn, "%s\r\n", s) }

		write("220 relay.test ESMTP")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					write("250 2.0.0 Ok")
				}
				continue
			}
			verb := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
				// No STARTTLS is advertised, so the notifier authenticates over this
				// connection and the credential is observable here.
				write("250-relay.test")
				write("250 AUTH PLAIN")
			case strings.HasPrefix(verb, "AUTH PLAIN"):
				fields := strings.Fields(line)
				if len(fields) >= 3 {
					select {
					case got <- fields[2]:
					default:
					}
				}
				write("235 2.7.0 Authentication successful")
			case strings.HasPrefix(verb, "MAIL FROM"), strings.HasPrefix(verb, "RCPT TO"):
				write("250 2.0.0 Ok")
			case strings.HasPrefix(verb, "DATA"):
				inData = true
				write("354 End data with <CR><LF>.<CR><LF>")
			case strings.HasPrefix(verb, "QUIT"):
				write("221 2.0.0 Bye")
				return
			default:
				write("250 2.0.0 Ok")
			}
		}
	}()
	return ln.Addr().String(), got
}
