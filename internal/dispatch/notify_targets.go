package dispatch

import (
	"encoding/json"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// notifyRunTargets delivers a terminal top-level run to the per-run notification targets it carries,
// in addition to the server-wide channels. Each target names a channel kind and a URL, so a template
// routes its runs to its own team. A target marked OnFailure is skipped for a successful run.
// Delivery reuses the built-in channel formatters and their bounded, best-effort sending.
func (d *Dispatcher) notifyRunTargets(r *run.Run) {
	if len(r.Notifications) == 0 {
		return
	}
	byKind := map[string][]string{}
	var richer []run.NotifyTarget
	for _, t := range r.Notifications {
		if !run.ValidNotifyKind(t.Kind) {
			continue
		}
		if t.OnFailure && r.Status == run.StatusSucceeded {
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
		if body := encode("webhook", notification{Event: "run.finished", Run: &redacted}); body != nil {
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
		headers := map[string]string{
			"Content-Type": "text/plain",
			"Title":        "SwitchTender run " + runLabel(r) + " " + string(r.Status),
			"Tags":         "white_check_mark",
		}
		if r.Status != run.StatusSucceeded {
			headers["Tags"] = "x"
			headers["Priority"] = "high"
		}
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
