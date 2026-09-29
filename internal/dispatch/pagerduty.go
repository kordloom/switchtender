package dispatch

import (
	"encoding/json"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// pagerDutyEndpoint is the PagerDuty Events API v2 enqueue URL. It is a package variable so a test can
// point it at a mock server.
var pagerDutyEndpoint = "https://events.pagerduty.com/v2/enqueue"

// WithPagerDuty triggers a PagerDuty incident through the Events API for each routing key when a
// top-level run fails, so an on-call responder is paged.
func WithPagerDuty(keys []string) Option {
	return func(c *config) { c.pagerdutyKeys = append([]string(nil), keys...) }
}

// pagerDutyEvent is the Events API v2 payload that triggers an incident.
type pagerDutyEvent struct {
	// RoutingKey is the integration key of the PagerDuty service to page.
	RoutingKey string `json:"routing_key"`
	// EventAction is trigger for a new incident.
	EventAction string `json:"event_action"`
	// DedupKey collapses repeated events for the same run into one incident.
	DedupKey string `json:"dedup_key"`
	// Payload carries the incident detail.
	Payload pagerDutyPayload `json:"payload"`
}

// pagerDutyPayload is the incident detail PagerDuty requires.
type pagerDutyPayload struct {
	// Summary is the one-line incident title.
	Summary string `json:"summary"`
	// Source names the system the incident came from.
	Source string `json:"source"`
	// Severity is the incident severity, error for a failed run.
	Severity string `json:"severity"`
}

// notifyPagerDuty triggers an incident for a failed or interrupted top-level run on every configured
// routing key. A succeeded or canceled run is not an actionable incident, so it pages no one.
func (d *Dispatcher) notifyPagerDuty(r *run.Run) {
	if len(d.pagerdutyKeys) == 0 {
		return
	}
	if r.Status != run.StatusFailed && r.Status != run.StatusInterrupted {
		return
	}
	summary := "SwitchTender run " + runLabel(r) + " " + string(r.Status)
	if r.Error != "" {
		summary += ": " + truncateError(r.Error)
	}
	for _, key := range d.pagerdutyKeys {
		body, err := json.Marshal(pagerDutyEvent{
			RoutingKey: key, EventAction: "trigger", DedupKey: r.ID,
			Payload: pagerDutyPayload{Summary: summary, Source: "switchtender", Severity: "error"},
		})
		if err != nil {
			d.log.Error("dispatch: encode pagerduty event: "+err.Error(), zap.String("run_id", r.ID))
			continue
		}
		d.notifyWG.Add(1)
		go func(b []byte) {
			defer d.notifyWG.Done()
			d.deliver(pagerDutyEndpoint, r.ID, b)
		}(body)
	}
}
