package dispatch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// outboxFlush bounds the last delivery pass a dispatcher makes as it shuts down.
const outboxFlush = 5 * time.Second

// WithNotificationOutbox delivers each top-level run's events to the named targets attached to what
// it came from through the outbox, instead of the router's direct, best-effort delivery: each event
// is recorded in the run's order as it happens, and delivered per target and per run in that order,
// retried, and kept as failed when its attempts run out. Every process given an outbox on the same
// store delivers from it, and none delivers an event twice.
func WithNotificationOutbox(o *named.Outbox) Option {
	return func(c *config) { c.outbox = o }
}

// startOutbox starts delivering from the outbox, when one is configured, until the dispatcher
// closes, and starts the sweep of owed run ends when the store keeps a ledger of them.
func (d *Dispatcher) startOutbox() {
	if d.outbox == nil {
		return
	}
	d.notifyWG.Add(1)
	go func() {
		defer d.notifyWG.Done()
		d.outbox.Serve(d.ctx, d.sendNamed)
	}()
	if ledger, ok := d.store.(run.EndLedger); ok {
		d.notifyWG.Add(1)
		go d.watchEnds(ledger)
	}
}

// flushOutbox makes one last bounded delivery pass once the runs have stopped, so the events their
// shutdown recorded go out before the process exits when they can. Whatever it does not reach stays
// recorded for the next process.
func (d *Dispatcher) flushOutbox() {
	if d.outbox == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), outboxFlush)
	defer cancel()
	d.outbox.Flush(ctx, d.sendNamed)
}

// recordNamed records the event a top-level run is at for its named targets, as the run's next
// event, placed within the workflow by branch. It runs on the path of the run, before the run moves
// on, because the order events are recorded in is the order they are delivered in: recording later,
// off the path, would let a fast run's end overtake its start. The snapshot is taken now and
// redacted for sending off the host, so a message delivered later still says what was true at the
// event and never carries what may not leave. It returns the error of an event the outbox could not
// record after its own retries, already logged, so a run's end can be left owed for the sweep.
func (d *Dispatcher) recordNamed(r *run.Run, branch named.Branch) error {
	snap := redactForExternal(r)
	if err := d.outbox.Record(d.ctx, &snap, branch); err != nil {
		d.log.Error("dispatch: record notification event: "+err.Error(), zap.String("run_id", r.ID))
		return err
	}
	return nil
}

// namedNotification is the JSON body a named webhook target receives: the event and the run every
// webhook receives, plus what identifies this delivery, so a receiver can put messages in the run's
// order and drop a repeat.
type namedNotification struct {
	// Event names what happened: run.started, run.held, or run.finished.
	Event string `json:"event"`
	// Run is the run as it stood at the event.
	Run *run.Run `json:"run"`
	// Delivery identifies the message.
	Delivery namedDelivery `json:"delivery"`
}

// namedDelivery identifies one message to a named target.
type namedDelivery struct {
	// ID is the delivery's idempotency key: the target, the run, and the event's sequence number.
	ID string `json:"id"`
	// Sequence is the event's position in the run's own sequence of events.
	Sequence int64 `json:"sequence"`
	// Note says, when set, that an earlier notification about this run to this target failed.
	Note string `json:"note,omitempty"`
}

// sendNamed makes one attempt at delivering one event of a run to one named target, through the
// same formatters every other channel uses, and reports what came of it. A failure no retry can
// fix, a channel this server has no transport for or a target that refuses the request outright, is
// marked permanent. The note, when set, is added to the message in each channel's own place for it.
func (d *Dispatcher) sendNamed(ctx context.Context, m named.Message) error {
	r, t := m.Run, m.Target
	jsonHeaders := map[string]string{"Content-Type": "application/json"}
	switch t.Kind {
	case run.NotifySlack, run.NotifyMattermost, run.NotifyRocketChat:
		return d.postNamedJSON(ctx, t.URL, slackPayload{Text: withNote(slackMessage(r), m.Note)},
			jsonHeaders)
	case run.NotifyWebhook:
		body := namedNotification{Event: webhookEvent(r), Run: r,
			Delivery: namedDelivery{ID: m.Delivery, Sequence: m.Seq, Note: m.Note}}
		return d.postNamedJSON(ctx, t.URL, body, map[string]string{
			"Content-Type": "application/json", "Idempotency-Key": m.Delivery})
	case run.NotifyDiscord:
		return d.postNamedJSON(ctx, t.URL,
			discordPayload{Content: withNote(discordMessage(r), m.Note)}, jsonHeaders)
	case run.NotifyTeams:
		card := teamsCardPayload(r)
		if m.Note != "" && len(card.Attachments) == 1 {
			card.Attachments[0].Content.Body = append(card.Attachments[0].Content.Body,
				map[string]any{"type": "TextBlock", "text": m.Note, "isSubtle": true, "wrap": true})
		}
		return d.postNamedJSON(ctx, t.URL, card, jsonHeaders)
	case run.NotifyNtfy:
		return d.postNamed(ctx, t.URL, []byte(withNote(ntfyBody(r), m.Note)), ntfyHeaders(r))
	case run.NotifyPagerDuty:
		event, ok := pagerDutyEventFor(r, t.Key)
		if !ok {
			return named.Permanent(fmt.Errorf("a %s run pages no one", r.Status))
		}
		if m.Note != "" {
			event.Payload.Summary += " (" + m.Note + ")"
		}
		return d.postNamedJSON(ctx, d.pagerDutyEndpoint, event, jsonHeaders)
	case run.NotifyGrafana:
		ann := grafanaAnnotationFor(r)
		ann.Text = withNote(ann.Text, m.Note)
		headers := map[string]string{"Content-Type": "application/json"}
		if t.Key != "" {
			headers["Authorization"] = "Bearer " + t.Key
		}
		return d.postNamedJSON(ctx, strings.TrimRight(t.URL, "/")+"/api/annotations", ann, headers)
	case run.NotifyTwilio:
		return d.textNamed(ctx, r, t.To, m.Note)
	case run.NotifyEmail:
		return d.mailNamed(ctx, r, t.To, m.Note)
	}
	return named.Permanent(fmt.Errorf("unknown notification kind %q", t.Kind))
}

// withNote appends the note to a message on a line of its own, or returns the message unchanged
// when there is none.
func withNote(msg, note string) string {
	if note == "" {
		return msg
	}
	return msg + "\n" + note
}

// textNamed texts one recipient through the server-held Twilio account.
func (d *Dispatcher) textNamed(ctx context.Context, r *run.Run, to, note string) error {
	if !d.twilioConfigured() {
		return named.Permanent(errors.New("the server has no Twilio account configured, " +
			"so it cannot send a text"))
	}
	message := twilioText(r)
	if note != "" {
		message += " " + note
	}
	endpoint := d.twilioBaseURL + "/2010-04-01/Accounts/" + url.PathEscape(d.twilioSID) +
		"/Messages.json"
	form := url.Values{"To": {to}, "From": {d.twilioFrom}, "Body": {message}}
	err := d.postNamed(ctx, endpoint, []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Authorization": "Basic " +
			base64.StdEncoding.EncodeToString([]byte(d.twilioSID+":"+d.twilioToken)),
	})
	if err != nil {
		// The account SID is in the endpoint's path, so the endpoint is masked out of the reason.
		return addressMasked(err, endpoint)
	}
	return nil
}

// mailNamed mails one recipient list through the server-held SMTP transport.
func (d *Dispatcher) mailNamed(ctx context.Context, r *run.Run, to, note string) error {
	if d.emailer == nil {
		return named.Permanent(errors.New("the server has no email transport " +
			"configured, so it cannot send mail"))
	}
	recipients := splitRecipients(to)
	if len(recipients) == 0 {
		return named.Permanent(errors.New("the target names no recipient"))
	}
	body := emailBody(r)
	if note != "" {
		body += "\n" + note + "\n"
	}
	return d.emailer.SendTo(ctx, recipients, emailSubject(r), body)
}

// postNamedJSON encodes v and posts it with postNamed.
func (d *Dispatcher) postNamedJSON(ctx context.Context, address string, v any,
	headers map[string]string) error {
	body, err := json.Marshal(v)
	if err != nil {
		return named.Permanent(fmt.Errorf("encode notification: %w", err))
	}
	return d.postNamed(ctx, address, body, headers)
}

// postNamed makes one attempt at posting a notification to a named target's address, through the
// same guarded client as every other channel, and reports what came of it. The outbox owns the
// retries, so this tries once. A response the target will answer the same way however often it is
// asked, any 4xx but a timeout, a too-early, or a rate limit, and a redirect, which is not
// followed, is permanent. The address is the credential for most channels, so the reason never
// quotes it.
func (d *Dispatcher) postNamed(ctx context.Context, address string, body []byte,
	headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return named.Permanent(fmt.Errorf("the target's address is not usable: %w",
			addressMasked(err, address)))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := d.notifyClient().Do(req)
	if err != nil {
		return addressMasked(err, address)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, notifyDrainLimit))
	_ = res.Body.Close()
	switch code := res.StatusCode; {
	case code < 300:
		return nil
	case code == http.StatusRequestTimeout, code == http.StatusTooEarly,
		code == http.StatusTooManyRequests, code >= 500:
		return fmt.Errorf("the target answered %d", code)
	default:
		return named.Permanent(fmt.Errorf("the target answered %d", code))
	}
}

// addressMasked returns err with every address it quotes masked to its scheme and host, still
// marked permanent when err was.
func addressMasked(err error, addresses ...string) error {
	masked := errors.New(util.MaskURLError(err, addresses...).Error())
	if named.IsPermanent(err) {
		return named.Permanent(masked)
	}
	return masked
}
