package dispatch

import (
	"github.com/kordloom/switchtender/internal/run"
)

// WithNtfy publishes a finished or held run to each ntfy topic URL, such as
// https://ntfy.sh/my-topic or a self-hosted server. token is an optional bearer credential for a
// protected topic, applied to every url; an empty token leaves the request unauthenticated.
func WithNtfy(urls []string, token string) Option {
	return func(c *config) {
		c.ntfyURLs = append([]string(nil), urls...)
		c.ntfyToken = token
	}
}

// notifyNtfy publishes a finished or held top-level run to every configured ntfy topic.
func (d *Dispatcher) notifyNtfy(r *run.Run) {
	if len(d.ntfyURLs) == 0 {
		return
	}
	headers := ntfyHeaders(r)
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

// ntfyHeaders builds the headers an ntfy message carries, shared by the server-wide topics and a
// run's own targets so the two cannot disagree. A failed run raises the priority so it surfaces
// above routine notifications, and so does a held one, since somebody has to act on it.
func ntfyHeaders(r *run.Run) map[string]string {
	headers := map[string]string{
		"Content-Type": "text/plain",
		"Title":        "SwitchTender run " + runLabel(r) + " " + string(r.Status),
		"Tags":         "white_check_mark",
	}
	switch {
	case r.Status == run.StatusPendingApproval:
		headers["Title"] = "SwitchTender run " + runLabel(r) + " is waiting for approval"
		headers["Tags"] = "hourglass"
		headers["Priority"] = "high"
	case r.Status != run.StatusSucceeded:
		headers["Tags"] = "x"
		headers["Priority"] = "high"
	}
	return headers
}

// ntfyBody renders the message body for an ntfy notification: the status, the elapsed time, and any
// failure detail, or for a held run what holds it and who asked. It carries no extra vars, so
// channel secrets are not exposed.
func ntfyBody(r *run.Run) string {
	// The title already says the run is waiting, so the body says what the title does not. It was
	// the clause the chat channels append to their own rendering of the run, which read here as the
	// rest of a sentence whose start was in the title.
	if r.Status == run.StatusPendingApproval {
		if detail := heldDetail(r); detail != "" {
			return detail
		}
		return "A person has to approve it before it runs."
	}
	msg := string(r.Status)
	if el := runElapsed(r); el != "" {
		msg += " in " + el
	}
	if r.Status != run.StatusSucceeded && r.Error != "" {
		msg += "\n" + truncateError(r.Error)
	}
	return msg
}
