package dispatch

import (
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
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
	if len(d.slackWebhooks) == 0 {
		return
	}
	body, err := json.Marshal(slackPayload{Text: slackMessage(r)})
	if err != nil {
		d.log.Error("dispatch: encode slack notification: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	for _, url := range d.slackWebhooks {
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
	label := r.Playbook
	if label == "" {
		label = r.Command
	}
	if label == "" {
		label = r.ID
	}
	msg := fmt.Sprintf("%s SwitchTender run *%s* %s", icon, label, r.Status)
	if d := runElapsed(r); d != "" {
		msg += " in " + d
	}
	if r.Status != run.StatusSucceeded && r.Error != "" {
		msg += "\n> " + truncateError(r.Error)
	}
	return msg
}

// runElapsed returns the run's wall-clock duration rounded to the second, or an empty string when
// the run did not record both a start and an end.
func runElapsed(r *run.Run) string {
	if r.StartedAt == nil || r.EndedAt == nil {
		return ""
	}
	return r.EndedAt.Sub(*r.StartedAt).Round(time.Second).String()
}

// truncateError shortens an error message so a Slack notification stays a summary, not a log dump.
func truncateError(s string) string {
	const limit = 200
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
