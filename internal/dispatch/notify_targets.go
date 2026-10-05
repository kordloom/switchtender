package dispatch

import (
	"encoding/json"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// notifyRunTargets delivers a finished or held top-level run to the notification targets it
// carries, in addition to the server-wide channels. Each target names a channel kind and a URL, so
// a template routes its runs to its own team. A target marked OnFailure is skipped for a successful
// run and for a held one. Delivery reuses the built-in channel formatters and their bounded,
// best-effort sending.
func (d *Dispatcher) notifyRunTargets(r *run.Run) {
	d.deliverTargets(r, r.Notifications)
}

// deliverTargets delivers a run to a list of notification targets through the built-in channel
// formatters, the list a run carries and the named targets attached to what it came from alike, so
// the two paths cannot disagree about what a channel is sent.
func (d *Dispatcher) deliverTargets(r *run.Run, targets []run.NotifyTarget) {
	if len(targets) == 0 {
		return
	}
	byKind := map[string][]string{}
	var richer []run.NotifyTarget
	// The list is bounded where it is written, and bounded again here so a run stored before that limit
	// existed cannot still fan out into a goroutine and a socket per target. What is dropped is named,
	// because silently delivering to some of a list reads as delivering to all of it.
	if len(targets) > run.MaxNotifyTargets {
		d.log.Warn("dispatch: notification targets truncated to the limit",
			zap.String("run_id", r.ID), zap.Int("carried", len(targets)),
			zap.Int("delivered", run.MaxNotifyTargets))
		targets = targets[:run.MaxNotifyTargets]
	}
	for _, t := range targets {
		if !run.ValidNotifyKind(t.Kind) {
			continue
		}
		// A target that asked for failures only hears neither a success nor a hold. It does hear an
		// attention alert, which reports trouble the way a failure does.
		if t.OnFailure && r.Attention == nil &&
			(r.Status == run.StatusSucceeded || r.Status == run.StatusPendingApproval) {
			continue
		}
		// A URL-configured channel groups by URL; a richer channel carries its own key or recipient
		// and is delivered one target at a time below.
		switch t.Kind {
		case run.NotifyPagerDuty, run.NotifyGrafana, run.NotifyTwilio, run.NotifyEmail:
			richer = append(richer, t)
		default:
			if t.URL != "" {
				byKind[t.Kind] = append(byKind[t.Kind], t.URL)
			}
		}
	}
	d.notifyRicherTargets(r, richer)

	postJSON := func(urls []string, body []byte) {
		for _, u := range urls {
			d.notifyWG.Add(1)
			go func(u string) {
				defer d.notifyWG.Done()
				d.deliver(u, r.ID, body)
			}(u)
		}
	}
	encode := func(kind string, v any) []byte {
		body, err := json.Marshal(v)
		if err != nil {
			d.log.Error("dispatch: encode "+kind+" notification: "+err.Error(),
				zap.String("run_id", r.ID))
			return nil
		}
		return body
	}

	if urls := byKind[run.NotifySlack]; len(urls) > 0 {
		d.deliverSlackFormat(urls, "slack", r)
	}
	if urls := byKind[run.NotifyMattermost]; len(urls) > 0 {
		d.deliverSlackFormat(urls, "mattermost", r)
	}
	if urls := byKind[run.NotifyRocketChat]; len(urls) > 0 {
		d.deliverSlackFormat(urls, "rocketchat", r)
	}
	if urls := byKind[run.NotifyWebhook]; len(urls) > 0 {
		// Redacted through the one helper the server-wide webhook uses, so the two paths cannot disagree
		// about what a webhook may see. Listing the fields here instead let this one keep the command,
		// which is the run's raw script body, while the server-wide webhook for the same run stripped it.
		redacted := redactForExternal(r)
		if body := encode("webhook", notification{Event: webhookEvent(r), Run: &redacted}); body != nil {
			postJSON(urls, body)
		}
	}
	if urls := byKind[run.NotifyDiscord]; len(urls) > 0 {
		if body := encode("discord", discordPayload{Content: discordMessage(r)}); body != nil {
			postJSON(urls, body)
		}
	}
	if urls := byKind[run.NotifyTeams]; len(urls) > 0 {
		if body := encode("teams", teamsCardPayload(r)); body != nil {
			postJSON(urls, body)
		}
	}
	if urls := byKind[run.NotifyNtfy]; len(urls) > 0 {
		headers := ntfyHeaders(r)
		body := []byte(ntfyBody(r))
		for _, u := range urls {
			d.notifyWG.Add(1)
			go func(u string) {
				defer d.notifyWG.Done()
				d.deliverWithHeaders(u, r.ID, body, headers)
			}(u)
		}
	}
}
