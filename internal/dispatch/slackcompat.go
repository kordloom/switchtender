package dispatch

import (
	"github.com/kordloom/switchtender/internal/run"
)

// WithMattermost posts a message to each Mattermost incoming webhook URL when a top-level run reaches
// a terminal state. Mattermost accepts the Slack-compatible payload.
func WithMattermost(urls []string) Option {
	return func(c *config) { c.mattermostWebhooks = append([]string(nil), urls...) }
}

// notifyMattermost posts a terminal top-level run to every configured Mattermost webhook.
func (d *Dispatcher) notifyMattermost(r *run.Run) {
	d.deliverSlackFormat(d.mattermostWebhooks, "mattermost", r)
}

// WithRocketChat posts a message to each Rocket.Chat incoming webhook URL when a top-level run reaches
// a terminal state. Rocket.Chat accepts the Slack-compatible payload.
func WithRocketChat(urls []string) Option {
	return func(c *config) { c.rocketChatWebhooks = append([]string(nil), urls...) }
}

// notifyRocketChat posts a terminal top-level run to every configured Rocket.Chat webhook.
func (d *Dispatcher) notifyRocketChat(r *run.Run) {
	d.deliverSlackFormat(d.rocketChatWebhooks, "rocketchat", r)
}
