package dispatch

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
)

// emailTimeout bounds one email delivery attempt.
const emailTimeout = 10 * time.Second

// Emailer sends a notification email. It is satisfied by SMTPEmailer and by test doubles.
type Emailer interface {
	// Send delivers a message with the given subject and body. It must honor the context deadline.
	Send(ctx context.Context, subject, body string) error
}

// WithEmail sends an email when a top-level run reaches a terminal state. When onFailureOnly is
// set, only failed runs notify; otherwise every finished run does.
func WithEmail(emailer Emailer, onFailureOnly bool) Option {
	return func(c *config) {
		c.emailer = emailer
		c.emailOnFailureOnly = onFailureOnly
	}
}

// notifyEmail sends a terminal top-level run to the configured emailer without blocking the
// executor. Failures are logged and dropped; the store remains the source of truth.
func (d *Dispatcher) notifyEmail(r *run.Run) {
	if d.emailer == nil {
		return
	}
	if d.emailOnFailureOnly && r.Status != run.StatusFailed {
		return
	}
	subject := fmt.Sprintf("Yardmaster run %s %s", r.ID, r.Status)
	body := emailBody(r)
	d.notifyWG.Add(1)
	go func() {
		defer d.notifyWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), emailTimeout)
		defer cancel()
		if err := d.emailer.Send(ctx, subject, body); err != nil {
			d.log.Warn("dispatch: email: "+err.Error(), zap.String("run_id", r.ID))
		}
	}()
}

// emailBody renders a short plain-text summary of a finished run.
func emailBody(r *run.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s finished with status %s.\n\n", r.ID, r.Status)
	fmt.Fprintf(&b, "Playbook: %s\n", r.Playbook)
	if r.Inventory != "" {
		fmt.Fprintf(&b, "Inventory: %s\n", r.Inventory)
	}
	if r.ExitCode != nil {
		fmt.Fprintf(&b, "Exit code: %d\n", *r.ExitCode)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", r.Error)
	}
	return b.String()
}

// SMTPEmailer sends notifications over SMTP with net/smtp. It dials with the call's context so a
// slow server cannot hang the notifier.
type SMTPEmailer struct {
	// addr is the SMTP server host:port.
	addr string
	// from is the envelope and header sender address.
	from string
	// to is the list of recipient addresses.
	to []string
	// auth is the SMTP authentication, nil for an unauthenticated relay.
	auth smtp.Auth
}

// NewSMTPEmailer returns an SMTPEmailer for the given server, sender, and recipients. auth may be
// nil for a server that needs no authentication.
func NewSMTPEmailer(addr, from string, to []string, auth smtp.Auth) *SMTPEmailer {
	return &SMTPEmailer{addr: addr, from: from, to: append([]string(nil), to...), auth: auth}
}

// Send delivers one message to every recipient, honoring the context deadline for the whole
// exchange.
func (e *SMTPEmailer) Send(ctx context.Context, subject, body string) error {
	host, _, err := net.SplitHostPort(e.addr)
	if err != nil {
		return fmt.Errorf("smtp address: %w", err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", e.addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Upgrade to TLS before authenticating so credentials and mail are never sent in the clear. Go's
	// smtp.PlainAuth also refuses to send on an unencrypted connection to any non-localhost relay, so
	// without this a real relay is unreachable.
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if e.auth != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(e.auth); err != nil {
				return fmt.Errorf("smtp auth: %w", err)
			}
		}
	}
	if err := client.Mail(e.from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, addr := range e.to {
		if err := client.Rcpt(addr); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", addr, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write([]byte(e.message(subject, body))); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return client.Quit()
}

// message builds the RFC 5322 message for subject and body.
func (e *SMTPEmailer) message(subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", e.from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(e.to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.String()
}
