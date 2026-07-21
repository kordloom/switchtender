package dispatch

import (
	"encoding/base64"
	"net/url"

	"github.com/dcadolph/switchtender/internal/run"
)

// twilioBaseURL is the Twilio REST API host. It is a package variable so a test can point it at a
// mock server.
var twilioBaseURL = "https://api.twilio.com"

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

// notifyTwilio texts each recipient about a failed or interrupted top-level run. A succeeded or
// canceled run is not an alert, so it texts no one. Every field must be set, or the channel is off.
func (d *Dispatcher) notifyTwilio(r *run.Run) {
	if d.twilioSID == "" || d.twilioToken == "" || d.twilioFrom == "" || len(d.twilioTo) == 0 {
		return
	}
	if r.Status != run.StatusFailed && r.Status != run.StatusInterrupted {
		return
	}
	message := "SwitchTender run " + runLabel(r) + " " + string(r.Status)
	if r.Error != "" {
		message += ": " + truncateError(r.Error)
	}
	endpoint := twilioBaseURL + "/2010-04-01/Accounts/" + url.PathEscape(d.twilioSID) + "/Messages.json"
	headers := map[string]string{
		"Content-Type":  "application/x-www-form-urlencoded",
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(d.twilioSID+":"+d.twilioToken)),
	}
	for _, to := range d.twilioTo {
		form := url.Values{"To": {to}, "From": {d.twilioFrom}, "Body": {message}}
		body := []byte(form.Encode())
		d.notifyWG.Add(1)
		go func(b []byte) {
			defer d.notifyWG.Done()
			d.deliverWithHeaders(endpoint, r.ID, b, headers)
		}(body)
	}
}
