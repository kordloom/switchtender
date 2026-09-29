package server

import (
	"fmt"
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

// restoreMaskedNotifications replaces redacted URLs and keys in a submitted target list with the
// stored values they stand for, so a caller that never saw the real URL cannot change where a
// channel points by echoing the mask back, and an editor that leaves a row untouched keeps it
// working. It reports an error when a masked row cannot be tied to one stored channel.
//
// Matching used to be by array position, which quietly broke the ordinary edit. A template with a
// Slack target and a webhook target, edited to remove the Slack row, submits one masked webhook row
// at position 0 against a stored Slack target at position 0: the kinds disagree, so the URL was
// cleared, the row failed validation, and the template saved with no notifications at all while
// answering "Saved." Every later run of it notified nobody.
//
// Matching is by kind instead, consuming stored channels of that kind in order, which is the only
// identity a masked row actually carries: the mask keeps the scheme and host and nothing else. When
// a kind's masked rows cannot pick out which stored channel they mean, because fewer rows came back
// than are stored and those channels differ, the request is refused rather than guessed at. Guessing
// there repoints alerts at the channel the operator just deleted, silently.
func restoreMaskedNotifications(incoming, stored []run.NotifyTarget) ([]run.NotifyTarget, error) {
	if len(incoming) == 0 {
		return incoming, nil
	}
	out := make([]run.NotifyTarget, len(incoming))
	copy(out, incoming)

	maskedPerKind := map[string]int{}
	for _, t := range out {
		if strings.Contains(t.URL, util.MaskMarker) || strings.Contains(t.Key, util.MaskMarker) {
			maskedPerKind[t.Kind]++
		}
	}
	storedPerKind := map[string][]run.NotifyTarget{}
	for _, t := range stored {
		storedPerKind[t.Kind] = append(storedPerKind[t.Kind], t)
	}
	for kind, masked := range maskedPerKind {
		have := storedPerKind[kind]
		if masked >= len(have) || allSameURL(have) {
			continue
		}
		return nil, fmt.Errorf("this template has %d %s channels with different addresses and the "+
			"edit sends %d of them with the address still redacted, so which channel to keep cannot "+
			"be told apart: re-enter the address for the %s channel you are keeping", len(have), kind,
			masked, kind)
	}

	taken := map[string]int{}
	for i := range out {
		maskedURL := strings.Contains(out[i].URL, util.MaskMarker)
		maskedKey := strings.Contains(out[i].Key, util.MaskMarker)
		if !maskedURL && !maskedKey {
			continue
		}
		have := storedPerKind[out[i].Kind]
		at := taken[out[i].Kind]
		if at < len(have) {
			taken[out[i].Kind]++
			if maskedURL {
				out[i].URL = have[at].URL
			}
			if maskedKey {
				out[i].Key = have[at].Key
			}
			continue
		}
		// A masked value with no stored counterpart is meaningless: clear it so the row is judged
		// on what it actually carries.
		if maskedURL {
			out[i].URL = ""
		}
		if maskedKey {
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
	return kept, nil
}

// allSameURL reports whether every target carries the same address, in which case a masked row
// cannot pick the wrong one however it is matched.
func allSameURL(targets []run.NotifyTarget) bool {
	for i := 1; i < len(targets); i++ {
		if targets[i].URL != targets[0].URL || targets[i].Key != targets[0].Key {
			return false
		}
	}
	return true
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
