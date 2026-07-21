package dispatch

import (
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// WithDiscord posts a formatted message to each Discord incoming webhook URL when a top-level run
// reaches a terminal state.
func WithDiscord(urls []string) Option {
	return func(c *config) { c.discordWebhooks = append([]string(nil), urls...) }
}

// discordPayload is the JSON body a Discord incoming webhook expects.
type discordPayload struct {
	// Content is the message posted to the channel, rendered with Discord markdown.
	Content string `json:"content"`
}

// notifyDiscord posts a terminal top-level run to every configured Discord webhook.
func (d *Dispatcher) notifyDiscord(r *run.Run) {
	if len(d.discordWebhooks) == 0 {
		return
	}
	body, err := json.Marshal(discordPayload{Content: discordMessage(r)})
	if err != nil {
		d.log.Error("dispatch: encode discord notification: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	for _, url := range d.discordWebhooks {
		d.notifyWG.Add(1)
		go func(u string) {
			defer d.notifyWG.Done()
			d.deliver(u, r.ID, body)
		}(url)
	}
}

// discordMessage renders a run as a one-line Discord message with a status emoji, the run label, and
// the elapsed time. It carries no extra vars, so channel secrets are not exposed.
func discordMessage(r *run.Run) string {
	icon := "✅"
	if r.Status != run.StatusSucceeded {
		icon = "❌"
	}
	msg := fmt.Sprintf("%s SwitchTender run **%s** %s", icon, runLabel(r), r.Status)
	if d := runElapsed(r); d != "" {
		msg += " in " + d
	}
	if r.Status != run.StatusSucceeded && r.Error != "" {
		msg += "\n> " + truncateError(r.Error)
	}
	return msg
}
