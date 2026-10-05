package dispatch

import (
	"encoding/base64"
	"net/url"

	"github.com/kordloom/switchtender/internal/run"
)

// defaultTwilioBaseURL is the Twilio REST API host. A dispatcher copies it into a field at
// construction so a test can point one instance at a mock server without a shared global.
const defaultTwilioBaseURL = "https://api.twilio.com"

// WithTwilio sends an SMS through the Twilio API to each recipient when a top-level run fails. sid and
// token authenticate the account, and from is the Twilio sender number.
func WithTwilio(sid, token, from string, to []string) Option {
	return func(c *config) {
		c.twilioSID = sid
		c.twilioToken = token
		c.twilioFrom = from
		c.twilioTo = append([]string(nil), to...)
	}
}

// notifyTwilio texts each server-wide recipient about a failed or interrupted top-level run. A
// succeeded or canceled run is not an alert, so it texts no one. Every field must be set, or the
// channel is off.
func (d *Dispatcher) notifyTwilio(r *run.Run) {
	if d.twilioSID == "" || d.twilioToken == "" || d.twilioFrom == "" || len(d.twilioTo) == 0 {
		return
	}
	if r.Status != run.StatusFailed && r.Status != run.StatusInterrupted {
		return
	}
	for _, to := range d.twilioTo {
		d.sendTwilioText(r, to)
	}
}

// twilioConfigured reports whether the server holds a Twilio account to send through. A template
// names only a recipient, so without an account there is nothing to carry a per-template text.
func (d *Dispatcher) twilioConfigured() bool {
	return d.twilioSID != "" && d.twilioToken != "" && d.twilioFrom != ""
}

// sendTwilioText posts one SMS to a single recipient through the configured account. The caller has
// already decided the run warrants a text and confirmed the account is set.
func (d *Dispatcher) sendTwilioText(r *run.Run, to string) {
	message := twilioText(r)
	endpoint := d.twilioBaseURL + "/2010-04-01/Accounts/" + url.PathEscape(d.twilioSID) + "/Messages.json"
	headers := map[string]string{
		"Content-Type":  "application/x-www-form-urlencoded",
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(d.twilioSID+":"+d.twilioToken)),
	}
	form := url.Values{"To": {to}, "From": {d.twilioFrom}, "Body": {message}}
	body := []byte(form.Encode())
	d.notifyWG.Add(1)
	go func() {
		defer d.notifyWG.Done()
		d.deliverWithHeaders(endpoint, r.ID, body, headers)
	}()
}

// twilioText is the text message a run sends: an attention alert's summary, a start, or the run's
// status with its error. Every Twilio path builds it here, so a phone reads the same message
// whichever path reaches it.
func twilioText(r *run.Run) string {
	if r.Attention != nil {
		return "SwitchTender alert: " + r.Attention.Summary
	}
	message := "SwitchTender run " + runLabel(r) + " " + string(r.Status)
	if r.Status == run.StatusRunning {
		message = "SwitchTender run " + runLabel(r) + " " + startedSentence
	}
	if r.Error != "" {
		message += ": " + truncateError(r.Error)
	}
	return message
}
