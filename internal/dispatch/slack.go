package dispatch

import (
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// WithSlack posts a formatted message to each Slack incoming webhook URL when a top-level run
// reaches a terminal state.
func WithSlack(urls []string) Option {
	return func(c *config) { c.slackWebhooks = append([]string(nil), urls...) }
}

// slackPayload is the JSON body a Slack incoming webhook expects.
type slackPayload struct {
	// Text is the message posted to the channel, rendered with Slack markup.
	Text string `json:"text"`
}

// notifySlack posts a terminal top-level run to every configured Slack webhook.
func (d *Dispatcher) notifySlack(r *run.Run) {
	d.deliverSlackFormat(d.slackWebhooks, "slack", r)
}

// deliverSlackFormat posts the Slack-compatible {"text": ...} payload to each url. Slack, Mattermost,
// and Rocket.Chat all accept an incoming webhook in that shape, so they share this delivery. label
// names the channel for a log message on an encoding failure.
func (d *Dispatcher) deliverSlackFormat(urls []string, label string, r *run.Run) {
	if len(urls) == 0 {
		return
	}
	body, err := json.Marshal(slackPayload{Text: slackMessage(r)})
	if err != nil {
		d.log.Error("dispatch: encode "+label+" notification: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	for _, url := range urls {
		d.notifyWG.Add(1)
		go func(u string) {
			defer d.notifyWG.Done()
			d.deliver(u, r.ID, body)
		}(url)
	}
}

// slackMessage renders a run as a one-line Slack message with a status icon, the run label, and
// the elapsed time. It carries no extra vars, so channel secrets are not exposed.
func slackMessage(r *run.Run) string {
	icon := ":white_check_mark:"
	if r.Status != run.StatusSucceeded {
		icon = ":x:"
	}
	msg := fmt.Sprintf("%s SwitchTender run *%s* %s", icon, runLabel(r), r.Status)
	if d := runElapsed(r); d != "" {
		msg += " in " + d
	}
	if r.Status != run.StatusSucceeded && r.Error != "" {
		msg += "\n> " + truncateError(r.Error)
	}
	return msg
}

// runLabel returns a human label for a run in a notification: its playbook, else its tool and id, so a
// channel message always names the run by whatever identifies it best.
//
// It never returns the command. A playbook is a path, and naming it tells a reader which change ran. A
// command is the run's whole script body, and this label is embedded in the message every external
// channel sends, so returning it put inline passwords and tokens into Slack, Discord, Teams, PagerDuty,
// ntfy, Grafana, and Twilio, against the rule the rest of the notification code keeps: the run store,
// the live stream, and the run page hold the command, and only what leaves the host loses it. A shell
// run is named by its tool and its id, which is what a reader needs to go and open it.
func runLabel(r *run.Run) string {
	if r.Playbook != "" {
		return r.Playbook
	}
	if tool := run.NormalizeTool(r.Tool); tool != "" && tool != run.ToolAnsible {
		return string(tool) + " " + r.ID
	}
	return r.ID
}

// runElapsed returns the run's wall-clock duration rounded to the second, or an empty string when
// the run did not record both a start and an end.
func runElapsed(r *run.Run) string {
	if r.StartedAt == nil || r.EndedAt == nil {
		return ""
	}
	return r.EndedAt.Sub(*r.StartedAt).Round(time.Second).String()
}

// truncateError shortens an error message so a notification stays a summary, not a log dump. The
// cut itself is the shared util.Clip.
func truncateError(s string) string {
	const limit = 200
	return util.Clip(s, limit)
}
