package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// WithDiscord posts a formatted message to each Discord incoming webhook URL when a top-level run
// finishes or is held for approval.
func WithDiscord(urls []string) Option {
	return func(c *config) { c.discordWebhooks = append([]string(nil), urls...) }
}

// discordPayload is the JSON body a Discord incoming webhook expects.
type discordPayload struct {
	// Content is the message posted to the channel, rendered with Discord markdown.
	Content string `json:"content"`
	// AllowedMentions tells Discord to ping nobody, whatever the content names.
	AllowedMentions discordNoMentions `json:"allowed_mentions"`
}

// discordNoMentions is a Discord allowed_mentions object with an empty parse list, which stops a
// message from pinging @everyone, @here, a role, or a user. Its zero value is the whole setting,
// so every payload built anywhere carries it without having to remember to.
type discordNoMentions struct{}

// MarshalJSON encodes the allowed_mentions object with an empty parse list.
func (discordNoMentions) MarshalJSON() ([]byte, error) {
	return []byte(`{"parse":[]}`), nil
}

// discordEscaper puts a backslash before each character Discord reads as markdown or as the start
// of a mention or a masked link. Escaping the brackets is enough to stop a masked link, so
// parentheses, which a policy name often holds, are left as written.
var discordEscaper = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "<", `\<`,
	"[", `\[`, "]", `\]`, "@", `\@`,
)

// discordEscape returns s with Discord's markdown escaped, so a value from outside, such as a
// playbook an agent named, cannot become a masked link, a spoiler, or a mention.
func discordEscape(s string) string {
	return discordEscaper.Replace(s)
}

// notifyDiscord posts a finished or held top-level run to every configured Discord webhook.
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
// the elapsed time. It carries no extra vars, so channel secrets are not exposed. Every value from
// outside goes through discordEscape first.
func discordMessage(r *run.Run) string {
	r = escapedNotice(r, discordEscape)
	if isSkip(r) {
		return "SwitchTender schedule **" + skipSchedule(r) + "** " + skipHeadline(r) + " " +
			skipDetail(r)
	}
	if r.Attention != nil {
		return "SwitchTender alert: " + attentionSentences(r)
	}
	if r.Status == run.StatusPendingApproval {
		if r.AwaitingStep != nil {
			return "SwitchTender workflow **" + runLabel(r) + "** " + stepSentences(r)
		}
		return "SwitchTender run **" + runLabel(r) + "** " + heldSentences(r)
	}
	if r.Status == run.StatusRunning {
		return "SwitchTender run **" + runLabel(r) + "** " + startedSentence
	}
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
