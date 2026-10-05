package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/safedial"
	"github.com/kordloom/switchtender/internal/util"
)

// webhookTimeout bounds one notification delivery attempt.
const webhookTimeout = 5 * time.Second

// notifyDrainLimit caps how much of a notification target's response is read before the body is
// closed. The response is not used for anything, only drained so the connection can be reused, and
// the target is named by whoever started the run, so what it may make the controller read is capped
// rather than trusted.
const notifyDrainLimit = 64 << 10

// WithWebhooks posts a JSON notification to each URL when a top-level run finishes or is held for
// approval.
func WithWebhooks(urls []string) Option {
	return func(c *config) { c.webhooks = append([]string(nil), urls...) }
}

// notification is the JSON body delivered to webhooks.
type notification struct {
	// Event names what happened: run.finished for a run that reached a terminal state, run.held for
	// one a rule is holding for a person to decide on, workflow.step_awaiting_approval for a workflow
	// waiting at one of its approval steps, or run.needs_attention for an attention alert.
	Event string `json:"event"`
	// Run is the run the event is about.
	Run *run.Run `json:"run"`
}

// webhookEvent names the event a webhook payload reports for r, so the server-wide and per-run
// webhook paths cannot disagree about it. A running run reaches a webhook only through a named
// target attached for the started event.
func webhookEvent(r *run.Run) string {
	switch {
	case isSkip(r):
		return skipWebhookEvent
	case r.Attention != nil:
		return eventNeedsAttention
	case r.Status == run.StatusPendingApproval && r.AwaitingStep != nil:
		return eventStepAwaiting
	}
	switch r.Status {
	case run.StatusPendingApproval:
		return "run.held"
	case run.StatusRunning:
		return "run.started"
	}
	return "run.finished"
}

// heldSentences is what a held notification says after each channel's own rendering of the run's
// label: that it waits, the rule holding it when one is named, and who asked for it when that is
// known.
func heldSentences(r *run.Run) string {
	msg := "is waiting for approval."
	if detail := heldDetail(r); detail != "" {
		msg += " " + detail
	}
	return msg
}

// heldDetail is what a held notification says beyond the fact of the hold, as whole sentences: the
// rule holding it when one is named, why the gate held a dry run it did not find change free and
// what would change that, and who asked for it when that is known. It is empty when none is
// recorded.
func heldDetail(r *run.Run) string {
	var detail []string
	if r.HeldByPolicy != "" {
		detail = append(detail, "Held by \""+r.HeldByPolicy+"\".")
	}
	// The person deciding reads this before opening the run, and "dry run" alone would tell them
	// there is nothing to decide. The hold note says it in full when the scan is why the rule held
	// the run, and names the fixes; otherwise the scan's summary says what the run may do.
	switch why := r.NotChangeFreeSummary(); {
	case r.HoldNote != "":
		detail = append(detail, r.HoldNote)
	case why != "":
		detail = append(detail, "Not a preview: "+why+".")
	}
	if r.Actor != "" {
		detail = append(detail, "Requested by "+r.Actor+".")
	}
	return strings.Join(detail, " ")
}

// notifyHeld tells the channels a top-level run is waiting for a person, at the moment it is held.
//
// A channel that hears about a run only when it finishes never hears about a held one, since a held
// run finishes only after somebody decides on it. The gate would stop the change and the person who
// could release it would not be told, so an agent's request, a plan over its destroy limit, or a
// production deploy would sit until somebody happened to open the Runs page. A hold is a request
// for a decision rather than an incident, so it goes where finished runs go, the chat channels,
// email, and webhooks, and never to a pager, a text message, or a dashboard annotation. Plugin
// notifiers keep receiving finished runs only, which is the contract they were written against.
func (d *Dispatcher) notifyHeld(r *run.Run) {
	d.notifyHeldOn(r, named.Branch{})
}

// notifyHeldOn is notifyHeld for a hold that comes from a workflow step, placed within the workflow
// by branch, which is how the named targets' delivery orders it against the workflow's other steps.
// The zero Branch is the run's own hold.
func (d *Dispatcher) notifyHeldOn(r *run.Run, branch named.Branch) {
	if r.ParentID != nil || r.Status != run.StatusPendingApproval {
		return
	}
	d.notifyWebhooks(r)
	d.notifySlack(r)
	d.notifyMattermost(r)
	d.notifyRocketChat(r)
	d.notifyDiscord(r)
	d.notifyTeams(r)
	d.notifyNtfy(r)
	d.notifyEmail(r)
	d.notifyRunTargets(r)
	if branch.Step == "" {
		d.announceOwed(r, run.OwedHold)
		return
	}
	d.notifyNamedOn(r, branch)
}

// Announce tells the channels about a run this process did not execute: one a relay worker
// started or finished, or an apply held on a worker's plan. It satisfies relay.Announcer, so the
// control node announces what the relay records the way it announces what it executes itself.
func (d *Dispatcher) Announce(r *run.Run) {
	switch r.Status {
	case run.StatusPendingApproval:
		d.notifyHeld(r)
	case run.StatusRunning:
		d.notifyStarted(r)
	default:
		d.notify(r)
	}
}

// notify delivers a terminal top-level run to every configured channel without blocking the
// executor. Failures are logged and dropped; the store remains the source of truth.
func (d *Dispatcher) notify(r *run.Run) {
	if r.ParentID != nil || !r.Status.Terminal() {
		return
	}
	d.notifyWebhooks(r)
	d.notifySlack(r)
	d.notifyMattermost(r)
	d.notifyRocketChat(r)
	d.notifyDiscord(r)
	d.notifyTeams(r)
	d.notifyNtfy(r)
	d.notifyPagerDuty(r)
	d.notifyGrafana(r)
	d.notifyTwilio(r)
	d.notifyEmail(r)
	d.notifyExtra(r)
	d.notifyRunTargets(r)
	d.announceEnd(r)
}

// redactForExternal returns a copy of r safe to send off the host to an external channel, a plugin
// notifier or a webhook. Survey answers and template vars can carry secrets, each notification
// target carries a routing key or API token, Command holds the raw script body of a bash, python,
// powershell, or go run, which can embed inline secrets or sensitive arguments, and Outputs holds the
// values a playbook published with set_stats, which is exactly how a stripped extra var, survey
// answer, or runtime-fetched token re-enters the run and is not covered by the run masker unless it
// was a registered credential. All four are cleared here so the two external paths stay in parity and
// neither forgets a field the other strips. The in-tenant run store, the SSE hub, and the run-detail
// API keep them; only what leaves the host loses them.
func redactForExternal(r *run.Run) run.Run {
	out := *r
	out.ExtraVars = nil
	out.Notifications = nil
	out.Command = ""
	out.Outputs = nil
	// A pipeline stores each step's raw script in Steps[].Command, the same place an inline secret
	// lands as the top-level Command, so blank each step's script too. This copies the slice rather
	// than editing in place, since out shares the caller's Steps and the caller's run must keep its
	// scripts; the non-secret step shape (name, tool, dry run) is preserved for the notification.
	if r.Steps != nil {
		out.Steps = make([]run.PipelineStep, len(r.Steps))
		for i, s := range r.Steps {
			s.Command = ""
			out.Steps[i] = s
		}
	}
	return out
}

// notifyExtra fans a terminal top-level run out to every registered Notifier, off the executor
// path. The run is redacted of extra vars and per-run notification targets first, since a
// registered channel is external and must receive neither survey answers or template vars that can
// carry secrets nor the target list, whose entries carry a routing key or API token. Each delivery
// is bounded and its failure logged and dropped, like the built-in channels.
func (d *Dispatcher) notifyExtra(r *run.Run) {
	if len(notifiers) == 0 {
		return
	}
	redacted := redactForExternal(r)
	for name, n := range notifiers {
		d.notifyWG.Add(1)
		go func(name string, n Notifier) {
			defer d.notifyWG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
			defer cancel()
			if err := n.Notify(ctx, &redacted); err != nil {
				d.log.Warn("dispatch: notifier: "+err.Error(),
					zap.String("run_id", r.ID), zap.String("notifier", name))
			}
		}(name, n)
	}
}

// notifyWebhooks posts a top-level run to every configured webhook, when it finishes or is held.
func (d *Dispatcher) notifyWebhooks(r *run.Run) {
	if len(d.webhooks) == 0 {
		return
	}
	redacted := redactForExternal(r)
	body, err := json.Marshal(notification{Event: webhookEvent(r), Run: &redacted})
	if err != nil {
		d.log.Error("dispatch: encode notification: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	for _, url := range d.webhooks {
		d.notifyWG.Add(1)
		go func(u string) {
			defer d.notifyWG.Done()
			d.deliver(u, r.ID, body)
		}(url)
	}
}

// deliver posts one JSON notification, retrying transient failures once.
func (d *Dispatcher) deliver(url, runID string, body []byte) {
	d.deliverWithHeaders(url, runID, body, map[string]string{"Content-Type": "application/json"})
}

// notifyClient returns the client used to deliver a notification. The default refuses an address
// that is link-local, unspecified, or this server itself, since the target is named by whoever
// started the run rather than by an administrator. A test that serves on loopback replaces it.
//
// The default is built once and shared by every delivery. Building one per delivery gave each its
// own http.Transport, and a transport that is dropped without closing keeps the connection it
// dialed idle for its idle timeout, along with the read and write goroutines that serve it. A
// measured hundred deliveries left three hundred goroutines and a hundred sockets alive that way,
// against three for a shared client, so a controller notifying on every finished run accumulated
// them for as long as runs kept finishing. Sharing also means a keep-alive connection to a webhook
// target is reused instead of a fresh handshake per run.
func (d *Dispatcher) notifyClient() *http.Client {
	d.notifyHTTPOnce.Do(func() {
		if d.notifyHTTP == nil {
			d.notifyHTTP = safedial.OffHostClient(webhookTimeout)
		}
	})
	return d.notifyHTTP
}

// deliverWithHeaders posts one notification with the given request headers, retrying transient
// failures once. The JSON channels use deliver; a channel like ntfy that sends a text body and
// custom headers uses this directly.
func (d *Dispatcher) deliverWithHeaders(url, runID string, body []byte, headers map[string]string) {
	// A notification target is a URL the server fetches on its own network, and unlike a secret
	// source or a project remote it is not an administrator's: notification targets ride along with a
	// run, so anyone who may start one may name the address. Without a refusal a target pointed at
	// the cloud metadata endpoint turned every finished run into a request for instance credentials,
	// delivered by the server to whoever set the target. The refusal also covers this server itself,
	// which is where the services that assume only local processes reach them are listening.
	client := d.notifyClient()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err == nil {
			// The body is read out before it is closed. Closing an unread body makes net/http tear
			// the connection down instead of returning it to the pool, so every notification paid a
			// fresh handshake to a target that answers with any body at all, which most do. The read
			// is bounded because the target is named by whoever started the run: the client timeout
			// caps how long it may stall, and this caps how much it may make the controller hold.
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, notifyDrainLimit))
			_ = res.Body.Close()
			if res.StatusCode < 300 {
				return
			}
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	// The address is the credential for these channels, and a Twilio endpoint carries the account
	// SID in its path, so neither the field nor the transport error's own text may show it. The
	// error is the half that is easy to miss: net/http wraps a failure in a *url.Error that
	// re-embeds the whole address, so masking the field alone leaves the secret in the same line.
	msg := "delivery failed"
	if lastErr != nil {
		msg = util.MaskURLError(lastErr, url).Error()
	}
	d.log.Warn("dispatch: webhook: "+msg,
		zap.String("run_id", runID), zap.String("url", util.MaskURL(url)))
}
