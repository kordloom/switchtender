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

// deliverPagerDutyTo triggers an incident on one routing key for a failed or interrupted run.
func (d *Dispatcher) deliverPagerDutyTo(r *run.Run, routingKey string) {
	if r.Status != run.StatusFailed && r.Status != run.StatusInterrupted {
		return
	}
	summary := "SwitchTender run " + runLabel(r) + " " + string(r.Status)
	if r.Error != "" {
		summary += ": " + truncateError(r.Error)
	}
	body, err := json.Marshal(pagerDutyEvent{
		RoutingKey: routingKey, EventAction: "trigger", DedupKey: r.ID,
		Payload: pagerDutyPayload{Summary: summary, Source: "switchtender", Severity: "error"},
	})
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
	ann := grafanaAnnotation{Text: grafanaText(r), Tags: []string{"switchtender", string(r.Status)}}
	if r.EndedAt != nil {
		ann.Time = r.EndedAt.UnixMilli()
	}
	body, err := json.Marshal(ann)
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
	subject := "SwitchTender run " + r.ID + " " + string(r.Status)
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
