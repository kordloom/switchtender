package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
)

// webhookTimeout bounds one notification delivery attempt.
const webhookTimeout = 5 * time.Second

// WithWebhooks posts a JSON notification to each URL when a top-level run reaches a terminal
// state.
func WithWebhooks(urls []string) Option {
	return func(c *config) { c.webhooks = append([]string(nil), urls...) }
}

// notification is the JSON body delivered to webhooks.
type notification struct {
	// Event names what happened; currently always run.finished.
	Event string `json:"event"`
	// Run is the terminal run.
	Run *run.Run `json:"run"`
}

// notify delivers a terminal top-level run to every configured webhook without blocking the
// executor. Failures are logged and dropped; the store remains the source of truth.
func (d *Dispatcher) notify(r *run.Run) {
	if len(d.webhooks) == 0 || r.ParentID != nil || !r.Status.Terminal() {
		return
	}
	body, err := json.Marshal(notification{Event: "run.finished", Run: r})
	if err != nil {
		d.log.Error("dispatch: encode notification: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	for _, url := range d.webhooks {
		go d.deliver(url, r.ID, body)
	}
}

// deliver posts one notification, retrying transient failures once.
func (d *Dispatcher) deliver(url, runID string, body []byte) {
	client := &http.Client{Timeout: webhookTimeout}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode < 300 {
				return
			}
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	msg := "delivery failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	d.log.Warn("dispatch: webhook: "+msg, zap.String("run_id", runID), zap.String("url", url))
}
