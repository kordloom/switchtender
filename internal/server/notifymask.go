package server

import (
	"net/url"
	"strings"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// maskMarker is the ellipsis a masked notification URL carries in place of its secret path. A
// submitted URL holding it is a redacted value coming back unchanged, not a new address.
const maskMarker = "…"

// maskNotifyURL redacts a notification URL for display. A webhook URL is a bearer secret: anyone
// holding it can post to the channel, so only the scheme and host survive a read.
func maskNotifyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return maskMarker
	}
	return u.Scheme + "://" + u.Host + "/" + maskMarker
}

// maskedNotifications returns a copy of the targets with their URLs redacted, for any response
// that carries a template.
func maskedNotifications(targets []run.NotifyTarget) []run.NotifyTarget {
	if len(targets) == 0 {
		return nil
	}
	out := make([]run.NotifyTarget, len(targets))
	for i, t := range targets {
		t.URL = maskNotifyURL(t.URL)
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
		if !strings.Contains(out[i].URL, maskMarker) {
			continue
		}
		if i < len(stored) && stored[i].Kind == out[i].Kind {
			out[i].URL = stored[i].URL
			continue
		}
		// A masked URL with no stored counterpart is meaningless, so drop the row rather than
		// storing an unusable address.
		out[i].URL = ""
	}
	kept := out[:0]
	for _, t := range out {
		if t.URL != "" {
			kept = append(kept, t)
		}
	}
	return kept
}
