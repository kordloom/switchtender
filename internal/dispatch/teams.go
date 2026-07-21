package dispatch

import (
	"encoding/json"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
)

// WithTeams posts an Adaptive Card to each Microsoft Teams incoming webhook URL when a top-level run
// reaches a terminal state.
func WithTeams(urls []string) Option {
	return func(c *config) { c.teamsWebhooks = append([]string(nil), urls...) }
}

// teamsMessage is the payload a Teams Workflows webhook accepts: one Adaptive Card attachment. The
// legacy Office 365 connector card is not used, since Microsoft retired connectors.
type teamsMessage struct {
	// Type is always message.
	Type string `json:"type"`
	// Attachments carries the single Adaptive Card.
	Attachments []teamsAttachment `json:"attachments"`
}

// teamsAttachment wraps the Adaptive Card content.
type teamsAttachment struct {
	// ContentType marks the attachment as an Adaptive Card.
	ContentType string `json:"contentType"`
	// Content is the card itself.
	Content teamsCard `json:"content"`
}

// teamsCard is the Adaptive Card body: a colored title block and a fact set of the run's outcome.
type teamsCard struct {
	// Schema is the Adaptive Card schema URL.
	Schema string `json:"$schema"`
	// Type is always AdaptiveCard.
	Type string `json:"type"`
	// Version is the card schema version.
	Version string `json:"version"`
	// Body holds the title block and the fact set.
	Body []map[string]any `json:"body"`
}

// notifyTeams posts a terminal top-level run to every configured Teams webhook.
func (d *Dispatcher) notifyTeams(r *run.Run) {
	if len(d.teamsWebhooks) == 0 {
		return
	}
	body, err := json.Marshal(teamsCardPayload(r))
	if err != nil {
		d.log.Error("dispatch: encode teams notification: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	for _, url := range d.teamsWebhooks {
		d.notifyWG.Add(1)
		go func(u string) {
			defer d.notifyWG.Done()
			d.deliver(u, r.ID, body)
		}(url)
	}
}

// teamsCardPayload renders a run as a Teams Adaptive Card: a title colored by outcome and a fact set
// with the status, the elapsed time, and any failure detail. It carries no extra vars, so channel
// secrets are not exposed.
func teamsCardPayload(r *run.Run) teamsMessage {
	color := "Good"
	if r.Status != run.StatusSucceeded {
		color = "Attention"
	}
	facts := []map[string]string{{"title": "Status", "value": string(r.Status)}}
	if el := runElapsed(r); el != "" {
		facts = append(facts, map[string]string{"title": "Elapsed", "value": el})
	}
	if r.Status != run.StatusSucceeded && r.Error != "" {
		facts = append(facts, map[string]string{"title": "Error", "value": truncateError(r.Error)})
	}
	card := teamsCard{
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Type:    "AdaptiveCard",
		Version: "1.4",
		Body: []map[string]any{
			{
				"type": "TextBlock", "text": "SwitchTender run " + runLabel(r),
				"weight": "Bolder", "size": "Medium", "color": color, "wrap": true,
			},
			{"type": "FactSet", "facts": facts},
		},
	}
	return teamsMessage{
		Type: "message",
		Attachments: []teamsAttachment{{
			ContentType: "application/vnd.microsoft.card.adaptive",
			Content:     card,
		}},
	}
}
