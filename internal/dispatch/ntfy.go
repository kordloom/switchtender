package dispatch

import (
	"github.com/kordloom/switchtender/internal/run"
)

// WithNtfy publishes a terminal run to each ntfy topic URL, such as https://ntfy.sh/my-topic or a
// self-hosted server. token is an optional bearer credential for a protected topic, applied to every
// url; an empty token leaves the request unauthenticated.
func WithNtfy(urls []string, token string) Option {
	return func(c *config) {
		c.ntfyURLs = append([]string(nil), urls...)
		c.ntfyToken = token
	}
}

// notifyNtfy publishes a terminal top-level run to every configured ntfy topic. A failed run raises
// the priority so it surfaces above routine notifications.
func (d *Dispatcher) notifyNtfy(r *run.Run) {
	if len(d.ntfyURLs) == 0 {
		return
	}
	headers := map[string]string{
		"Content-Type": "text/plain",
		"Title":        "SwitchTender run " + runLabel(r) + " " + string(r.Status),
		"Tags":         "white_check_mark",
	}
	if r.Status != run.StatusSucceeded {
		headers["Tags"] = "x"
		headers["Priority"] = "high"
	}
	if d.ntfyToken != "" {
		headers["Authorization"] = "Bearer " + d.ntfyToken
	}
	body := []byte(ntfyBody(r))
	for _, url := range d.ntfyURLs {
		d.notifyWG.Add(1)
		go func(u string) {
			defer d.notifyWG.Done()
			d.deliverWithHeaders(u, r.ID, body, headers)
		}(url)
	}
}

// ntfyBody renders the message body for an ntfy notification: the status, the elapsed time, and any
// failure detail. It carries no extra vars, so channel secrets are not exposed.
func ntfyBody(r *run.Run) string {
	msg := string(r.Status)
	if el := runElapsed(r); el != "" {
		msg += " in " + el
	}
	if r.Status != run.StatusSucceeded && r.Error != "" {
		msg += "\n" + truncateError(r.Error)
	}
	return msg
}
