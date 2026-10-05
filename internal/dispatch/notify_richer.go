package dispatch

import (
	"context"
	"encoding/json"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// notifyRicherTargets delivers the per-run notification targets that need more than a URL: a
// PagerDuty service, a Grafana instance, a Twilio recipient, or an email recipient.
//
// Each carries what it needs in the target itself, except the account secrets for Twilio and email,
// which stay server-held and are never stored in a target. A PagerDuty or Grafana target names its
// own key; a Twilio or email target names only a recipient, and the server account or SMTP transport
// carries the message. A target whose transport the server has not configured is logged and skipped,
// not silently dropped, so an operator can see a channel that could not fire.
func (d *Dispatcher) notifyRicherTargets(r *run.Run, targets []run.NotifyTarget) {
	for _, t := range targets {
		// A hold asks for a decision rather than reporting an incident, so it reaches an email
		// recipient and never pages, texts, or annotates a dashboard. A skipped schedule fire is
		// not an incident either, so it follows the same rule. An attention alert does report a
		// problem, and only a target attached for it reaches here with a pager or text kind.
		if ((r.Status == run.StatusPendingApproval && r.Attention == nil) || isSkip(r)) &&
			t.Kind != run.NotifyEmail {
			continue
		}
		switch t.Kind {
		case run.NotifyPagerDuty:
			d.deliverPagerDutyTo(r, t.Key)
		case run.NotifyGrafana:
			d.deliverGrafanaTo(r, t.URL, t.Key)
		case run.NotifyTwilio:
			d.deliverTwilioTo(r, t.To)
		case run.NotifyEmail:
			d.deliverEmailTo(r, t.To)
		}
	}
}

// pagerDutyEventFor builds the event a run triggers on one routing key: an incident for a failed or
// interrupted run, or a warning for an attention alert. It reports false for a run that pages for
// nothing. The per-target path and the named targets' ordered delivery both build it here, so a
// pager hears the same thing whichever path reaches it.
func pagerDutyEventFor(r *run.Run, routingKey string) (pagerDutyEvent, bool) {
	event := pagerDutyEvent{RoutingKey: routingKey, EventAction: "trigger", DedupKey: r.ID,
		Payload: pagerDutyPayload{Source: "switchtender", Severity: "error"}}
	switch {
	case r.Attention != nil:
		// The alert's own id collapses repeats of one condition into one incident, and a new
		// condition on the same run opens its own.
		event.DedupKey = r.Attention.ID
		event.Payload.Summary = r.Attention.Summary
		event.Payload.Severity = "warning"
	case r.Status != run.StatusFailed && r.Status != run.StatusInterrupted:
		return pagerDutyEvent{}, false
	default:
		event.Payload.Summary = "SwitchTender run " + runLabel(r) + " " + string(r.Status)
		if r.Error != "" {
			event.Payload.Summary += ": " + truncateError(r.Error)
		}
	}
	return event, true
}

// deliverPagerDutyTo triggers an incident on one routing key for a failed or interrupted run, or a
// warning for an attention alert.
func (d *Dispatcher) deliverPagerDutyTo(r *run.Run, routingKey string) {
	event, ok := pagerDutyEventFor(r, routingKey)
	if !ok {
		return
	}
	body, err := json.Marshal(event)
	if err != nil {
		d.log.Error("dispatch: encode pagerduty target: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	d.notifyWG.Add(1)
	go func() {
		defer d.notifyWG.Done()
		d.deliver(d.pagerDutyEndpoint, r.ID, body)
	}()
}

// deliverGrafanaTo posts one annotation to a Grafana instance with the target's own token.
func (d *Dispatcher) deliverGrafanaTo(r *run.Run, base, token string) {
	body, err := json.Marshal(grafanaAnnotationFor(r))
	if err != nil {
		d.log.Error("dispatch: encode grafana target: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	endpoint := strings.TrimRight(base, "/") + "/api/annotations"
	d.notifyWG.Add(1)
	go func() {
		defer d.notifyWG.Done()
		d.deliverWithHeaders(endpoint, r.ID, body, headers)
	}()
}

// deliverTwilioTo texts one recipient through the server-held Twilio account, reusing the same wire
// format as the server-wide channel. Without a configured account there is nothing to send through,
// so the target is logged and skipped rather than silently dropped.
func (d *Dispatcher) deliverTwilioTo(r *run.Run, to string) {
	if !d.twilioConfigured() {
		d.log.Warn("dispatch: a run names a twilio target but the server has no twilio account "+
			"configured, so it cannot send", zap.String("run_id", r.ID))
		return
	}
	d.sendTwilioText(r, to)
}

// deliverEmailTo mails one recipient list through the server-held SMTP transport.
func (d *Dispatcher) deliverEmailTo(r *run.Run, to string) {
	if d.emailer == nil {
		d.log.Warn("dispatch: a run names an email target but the server has no email transport "+
			"configured, so it cannot send", zap.String("run_id", r.ID))
		return
	}
	recipients := splitRecipients(to)
	if len(recipients) == 0 {
		return
	}
	subject := emailSubject(r)
	body := emailBody(r)
	d.notifyWG.Add(1)
	go func() {
		defer d.notifyWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), emailTimeout)
		defer cancel()
		if err := d.emailer.SendTo(ctx, recipients, subject, body); err != nil {
			d.log.Error("dispatch: send email target: "+err.Error(), zap.String("run_id", r.ID))
		}
	}()
}

// splitRecipients turns a comma-separated address list into a trimmed, non-empty slice.
func splitRecipients(to string) []string {
	var out []string
	for _, addr := range strings.Split(to, ",") {
		if a := strings.TrimSpace(addr); a != "" {
			out = append(out, a)
		}
	}
	return out
}
