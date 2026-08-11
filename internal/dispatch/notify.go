package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/safedial"
)

// webhookTimeout bounds one notification delivery attempt.
const webhookTimeout = 5 * time.Second

// WithWebhooks posts a JSON notification to each URL when a top-level run reaches a terminal
// state.
func WithWebhooks(urls []string) Option {
	return func(c *config) { c.webhooks = append([]string(nil), urls...) }
}

// notification is the JSON body delivered to webhooks.
type notification struct {
	// Event names what happened; currently always run.finished.
	Event string `json:"event"`
	// Run is the terminal run.
	Run *run.Run `json:"run"`
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
}

// notifyExtra fans a terminal top-level run out to every registered Notifier, off the executor
// path. The run is redacted of extra vars and per-run notification targets first, since a registered
// channel is external and must receive neither survey answers or template vars that can carry
// secrets nor the target list, whose entries carry a routing key or API token. Each delivery is
// bounded and its failure logged and dropped, like the built-in channels.
// redactForExternal returns a copy of r safe to send off the host to an external channel, a plugin
// notifier or a webhook. Survey answers and template vars can carry secrets, each notification
// target carries a routing key or API token, and Command holds the raw script body of a bash,
// python, powershell, or go run, which can embed inline secrets or sensitive arguments. All three
// are cleared here so the two external paths stay in parity and neither forgets a field the other
// strips. The in-tenant run store, the SSE hub, and the run-detail API keep Command; only what
// leaves the host loses it.
func redactForExternal(r *run.Run) run.Run {
	out := *r
	out.ExtraVars = nil
	out.Notifications = nil
	out.Command = ""
	return out
}

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

// notifyWebhooks posts a terminal top-level run to every configured webhook.
func (d *Dispatcher) notifyWebhooks(r *run.Run) {
	if len(d.webhooks) == 0 {
		return
	}
	redacted := redactForExternal(r)
	body, err := json.Marshal(notification{Event: "run.finished", Run: &redacted})
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

// deliverWithHeaders posts one notification with the given request headers, retrying transient
// failures once. The JSON channels use deliver; a channel like ntfy that sends a text body and
// custom headers uses this directly.
func (d *Dispatcher) deliverWithHeaders(url, runID string, body []byte, headers map[string]string) {
	// A notification target is an operator-supplied URL the server fetches on its own network, the
	// same shape as a secret source or a project remote, so it gets the same refusal: a resolved
	// address that is link-local or unspecified is not dialed. Without it a per-run notification
	// target pointed at the cloud metadata endpoint turned every finished run into a request for
	// instance credentials, delivered by the server to whoever set the target.
	client := safedial.Client(webhookTimeout)
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
			_ = res.Body.Close()
			if res.StatusCode < 300 {
				return
			}
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	msg := "delivery failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	d.log.Warn("dispatch: webhook: "+msg, zap.String("run_id", runID), zap.String("url", url))
}
