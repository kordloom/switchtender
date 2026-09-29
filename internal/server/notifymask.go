package server

import (
	"strings"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
)

// maskedNotifications returns a copy of the targets with their URLs redacted, for any response
// that carries a template.
func maskedNotifications(targets []run.NotifyTarget) []run.NotifyTarget {
	if len(targets) == 0 {
		return nil
	}
	out := make([]run.NotifyTarget, len(targets))
	for i, t := range targets {
		t.URL = util.MaskURL(t.URL)
		// The per-service key is a bearer secret too: a PagerDuty routing key pages the service and
		// a Grafana token writes annotations. It is redacted on read the same as a URL. The
		// recipient a twilio or email target names is not a secret and is left readable.
		if t.Key != "" {
			t.Key = util.MaskMarker
		}
		out[i] = t
	}
	return out
}

// maskTemplate returns a shallow copy of the template with its notification URLs redacted, so a
// read never hands back a channel secret.
func maskTemplate(t *template.Template) *template.Template {
	if t == nil {
		return nil
	}
	cp := *t
	cp.Notifications = maskedNotifications(t.Notifications)
	return &cp
}

// maskTemplates masks a whole list for a list response.
func maskTemplates(list []*template.Template) []*template.Template {
	out := make([]*template.Template, len(list))
	for i, t := range list {
		out[i] = maskTemplate(t)
	}
	return out
}

// restoreMaskedNotifications replaces redacted URLs in a submitted target list with the stored
// values they stand for, matched by position and channel kind. A caller that never saw the real
// URL therefore cannot change where a channel points by echoing the mask back, and an editor that
// leaves a row untouched keeps it working.
func restoreMaskedNotifications(incoming, stored []run.NotifyTarget) []run.NotifyTarget {
	if len(incoming) == 0 {
		return incoming
	}
	out := make([]run.NotifyTarget, len(incoming))
	copy(out, incoming)
	for i := range out {
		// A field arriving as the mask marker is a redacted value coming back unchanged from an
		// editor, so it is restored from the stored target beside it rather than saved as the
		// ellipsis. This applies to the URL and to the per-service key, the two masked fields.
		if i < len(stored) && stored[i].Kind == out[i].Kind {
			if strings.Contains(out[i].URL, util.MaskMarker) {
				out[i].URL = stored[i].URL
			}
			if strings.Contains(out[i].Key, util.MaskMarker) {
				out[i].Key = stored[i].Key
			}
			continue
		}
		// A masked value with no stored counterpart is meaningless: clear it so the row is judged
		// on what it actually carries.
		if strings.Contains(out[i].URL, util.MaskMarker) {
			out[i].URL = ""
		}
		if strings.Contains(out[i].Key, util.MaskMarker) {
			out[i].Key = ""
		}
	}
	// A row that is not deliverable is dropped rather than stored as one that will silently reach no
	// one. This is what previously dropped a row with an emptied URL, generalized to every kind.
	kept := out[:0]
	for _, t := range out {
		if run.ValidateNotifyTarget(t) == nil {
			kept = append(kept, t)
		}
	}
	return kept
}

// maskRun returns a shallow copy of the run with its notification URLs redacted.
//
// A template's targets were masked and a run's were not, and a template hands its targets to every
// run it launches. So the same Slack or webhook URL an admin sees as a redaction came back in full
// from the run endpoints, which any viewer may read. Masking one of the two places a value appears
// is not masking it.
func maskRun(rn *run.Run) *run.Run {
	if rn == nil || len(rn.Notifications) == 0 {
		return rn
	}
	cp := *rn
	cp.Notifications = maskedNotifications(rn.Notifications)
	return &cp
}

// maskRuns masks a whole list for a list response.
func maskRuns(list []*run.Run) []*run.Run {
	out := make([]*run.Run, len(list))
	for i, rn := range list {
		out[i] = maskRun(rn)
	}
	return out
}
