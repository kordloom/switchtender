package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
)

// WithGrafana posts an annotation to each Grafana base URL when a top-level run reaches a terminal
// state, so a run shows up as a marker on dashboards. token is the bearer credential for the Grafana
// annotations API, applied to every url.
func WithGrafana(urls []string, token string) Option {
	return func(c *config) {
		c.grafanaURLs = append([]string(nil), urls...)
		c.grafanaToken = token
	}
}

// grafanaAnnotation is the body the Grafana annotations API accepts. Time is the annotation instant in
// epoch milliseconds, omitted when the run recorded no end so Grafana stamps it on receipt.
type grafanaAnnotation struct {
	// Text is the annotation body shown on the dashboard.
	Text string `json:"text"`
	// Tags label the annotation for filtering, switchtender and the run status.
	Tags []string `json:"tags"`
	// Time is the annotation instant in epoch milliseconds.
	Time int64 `json:"time,omitempty"`
}

// notifyGrafana posts an annotation for a terminal top-level run to every configured Grafana instance.
func (d *Dispatcher) notifyGrafana(r *run.Run) {
	if len(d.grafanaURLs) == 0 {
		return
	}
	ann := grafanaAnnotation{Text: grafanaText(r), Tags: []string{"switchtender", string(r.Status)}}
	if r.EndedAt != nil {
		ann.Time = r.EndedAt.UnixMilli()
	}
	body, err := json.Marshal(ann)
	if err != nil {
		d.log.Error("dispatch: encode grafana annotation: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if d.grafanaToken != "" {
		headers["Authorization"] = "Bearer " + d.grafanaToken
	}
	for _, base := range d.grafanaURLs {
		url := strings.TrimRight(base, "/") + "/api/annotations"
		d.notifyWG.Add(1)
		go func(u string) {
			defer d.notifyWG.Done()
			d.deliverWithHeaders(u, r.ID, body, headers)
		}(url)
	}
}

// grafanaText renders a run as a one-line annotation with the run label, status, and elapsed time.
func grafanaText(r *run.Run) string {
	text := fmt.Sprintf("SwitchTender run %s %s", runLabel(r), r.Status)
	if el := runElapsed(r); el != "" {
		text += " in " + el
	}
	if r.Status != run.StatusSucceeded && r.Error != "" {
		text += ": " + truncateError(r.Error)
	}
	return text
}
