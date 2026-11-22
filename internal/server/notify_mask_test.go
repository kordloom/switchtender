package server

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// TestNotifyKeyIsMaskedOnRead pins that a per-service key never leaves on a read, the same as a
// channel URL, because a PagerDuty routing key pages the service and a Grafana token writes
// annotations, and both are readable by anyone who may read the template.
func TestNotifyKeyIsMaskedOnRead(t *testing.T) {
	t.Parallel()
	targets := []run.NotifyTarget{
		{Kind: run.NotifyPagerDuty, Key: "R0UTINGKEYsecret"},
		{Kind: run.NotifyGrafana, URL: "https://grafana.example.com/api/annotations", Key: "glsa_tok3n"},
		{Kind: run.NotifyTwilio, To: "+15550100"},
		{Kind: run.NotifyEmail, To: "oncall@example.com"},
	}
	masked := maskedNotifications(targets)
	joined := ""
	for _, t := range masked {
		joined += t.Kind + " url=" + t.URL + " key=" + t.Key + " to=" + t.To + "\n"
	}
	for _, secret := range []string{"R0UTINGKEYsecret", "glsa_tok3n"} {
		if strings.Contains(joined, secret) {
			t.Errorf("a per-service key leaked on read: %s", joined)
		}
	}
	// The recipient a twilio or email target names is not a secret and stays readable, so a person
	// can see where a channel goes.
	if !strings.Contains(joined, "+15550100") || !strings.Contains(joined, "oncall@example.com") {
		t.Errorf("a recipient was masked, so a reader cannot see where the channel goes: %s", joined)
	}
	// The grafana annotation host survives so a reader knows which instance it hits.
	if !strings.Contains(joined, "grafana.example.com") {
		t.Errorf("the grafana host was masked away: %s", joined)
	}
}

// TestRestoreKeepsRicherChannels checks the edit round-trip: a masked key comes back as the marker
// and is restored, and a pagerduty/twilio/email row is not dropped for having no URL.
func TestRestoreKeepsRicherChannels(t *testing.T) {
	t.Parallel()
	stored := []run.NotifyTarget{
		{Kind: run.NotifyPagerDuty, Key: "real-routing-key"},
		{Kind: run.NotifyTwilio, To: "+15550100"},
		{Kind: run.NotifyEmail, To: "team@example.com"},
	}
	// What an editor sends back after loading the masked template and changing nothing.
	incoming := []run.NotifyTarget{
		{Kind: run.NotifyPagerDuty, Key: util.MaskMarker},
		{Kind: run.NotifyTwilio, To: "+15550100"},
		{Kind: run.NotifyEmail, To: "team@example.com"},
	}
	got, err := restoreMaskedNotifications(incoming, stored)
	if err != nil {
		t.Fatalf("restoreMaskedNotifications() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("restore kept %d of 3 targets, so a richer channel was dropped: %+v", len(got), got)
	}
	pd := got[0]
	if pd.Kind != run.NotifyPagerDuty || pd.Key != "real-routing-key" {
		t.Errorf("the pagerduty key was not restored from storage: %+v", pd)
	}
	if !slices.ContainsFunc(got, func(t run.NotifyTarget) bool {
		return t.Kind == run.NotifyTwilio && t.To == "+15550100"
	}) {
		t.Error("the twilio recipient was dropped")
	}
}

// TestEveryRunResponseIsMasked pins that no handler answers with a run carrying live notification
// secrets. It reads the handler sources rather than driving each endpoint, because the defect it
// guards against is a new handler forgetting the mask, and a test that drives only today's endpoints
// would not see tomorrow's.
//
// Three paths shipped unmasked: the template launch response, the proposed-run apply response, and
// the webhook dedupe response, which answers an unauthenticated sender. Each returned the run with
// its Slack or PagerDuty target intact, so a caller holding only permission to fire a template could
// read the channel secrets behind it.
func TestEveryRunResponseIsMasked(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	// A response carrying one of these values is a run, and a run leaves only through maskRun.
	runValues := regexp.MustCompile(`respondJSON\([^)]*?,\s*(created|existing|launched|rn|got)\s*,`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", f, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !runValues.MatchString(line) {
				continue
			}
			if !strings.Contains(line, "maskRun(") {
				t.Errorf("%s answers with an unmasked run, so its notification targets leak:\n  %s",
					f, strings.TrimSpace(line))
			}
		}
	}
}

// TestRemovingOneNotificationKeepsTheOthers proves the ordinary edit works. Matching a masked row to
// a stored channel by array position meant that removing one row shifted every row that followed
// onto a stored channel of a different kind, whose URL was then cleared and whose row was dropped:
// a template with a Slack channel and a webhook, edited to remove the Slack one, saved with no
// notifications at all and answered "Saved." Every later run of it notified nobody.
func TestRemovingOneNotificationKeepsTheOthers(t *testing.T) {
	t.Parallel()
	stored := []run.NotifyTarget{
		{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/real/slack/url", OnFailure: true},
		{Kind: run.NotifyWebhook, URL: "https://ops.example.com/real/webhook", OnFailure: true},
	}
	// The editor loaded both rows masked, the operator removed the Slack row, and the webhook row
	// comes back with its address still redacted because the editor never had the real one.
	incoming := []run.NotifyTarget{
		{Kind: run.NotifyWebhook, URL: util.MaskURL(stored[1].URL), OnFailure: true},
	}
	got, err := restoreMaskedNotifications(incoming, stored)
	if err != nil {
		t.Fatalf("restoreMaskedNotifications() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("restore kept %d targets, want the webhook that survived the edit: %+v", len(got), got)
	}
	if got[0].Kind != run.NotifyWebhook || got[0].URL != stored[1].URL {
		t.Errorf("the surviving webhook = %+v, want its own stored address restored", got[0])
	}
}

// TestAnUndecidableMaskedEditIsRefused proves the case that cannot be matched is refused rather than
// guessed at. Two channels of one kind mask to the same scheme and host, so a single masked row
// coming back cannot say which one the operator kept; consuming them in order would silently
// repoint the surviving alerts at the channel that was just deleted.
func TestAnUndecidableMaskedEditIsRefused(t *testing.T) {
	t.Parallel()
	stored := []run.NotifyTarget{
		{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/aaa/deleted", OnFailure: true},
		{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/bbb/kept", OnFailure: true},
	}
	incoming := []run.NotifyTarget{
		{Kind: run.NotifySlack, URL: util.MaskURL(stored[1].URL), OnFailure: true},
	}
	if _, err := restoreMaskedNotifications(incoming, stored); err == nil {
		t.Error("an edit that cannot say which of two identical-looking channels it kept was accepted")
	}

	// Two channels that carry the same address are indistinguishable in every sense, so keeping one
	// of them is unambiguous and still works.
	same := []run.NotifyTarget{
		{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/one/hook", OnFailure: true},
		{Kind: run.NotifySlack, URL: "https://hooks.slack.com/services/one/hook", OnFailure: true},
	}
	got, err := restoreMaskedNotifications(
		[]run.NotifyTarget{{Kind: run.NotifySlack, URL: util.MaskURL(same[0].URL), OnFailure: true}}, same)
	if err != nil {
		t.Fatalf("identical channels should not be ambiguous: %v", err)
	}
	if len(got) != 1 || got[0].URL != same[0].URL {
		t.Errorf("restore = %+v, want the shared address restored", got)
	}
}

// TestRestoreKeepsAGrafanaTokenOnlyBesideItsAddress proves a template's inline Grafana row gets its
// stored token back only when its address is masked or is the stored one. A masked token beside a
// new address would carry the token to a host the editor chose. A PagerDuty key is restored as
// before.
func TestRestoreKeepsAGrafanaTokenOnlyBesideItsAddress(t *testing.T) {
	t.Parallel()
	const storedURL = "https://grafana.corp/api/annotations"
	mask := "https://grafana.corp/" + util.MaskMarker
	stored := []run.NotifyTarget{{Kind: run.NotifyGrafana, URL: storedURL, Key: "K"},
		{Kind: run.NotifyPagerDuty, Key: "PD"}}
	tests := []struct {
		// In is the submitted row.
		In run.NotifyTarget
		// WantErr reports whether the restore is refused.
		WantErr bool
		// WantKey is the restored key when it is not.
		WantKey string
	}{{ // Test 0: A new address beside the masked token is refused.
		In:      run.NotifyTarget{Kind: run.NotifyGrafana, URL: "https://other.example", Key: util.MaskMarker},
		WantErr: true,
	}, { // Test 1: The masked address and masked token restore both.
		In:      run.NotifyTarget{Kind: run.NotifyGrafana, URL: mask, Key: util.MaskMarker},
		WantKey: "K",
	}, { // Test 2: The stored address with the masked token restores the token.
		In:      run.NotifyTarget{Kind: run.NotifyGrafana, URL: storedURL, Key: util.MaskMarker},
		WantKey: "K",
	}, { // Test 3: A PagerDuty row with the masked key restores the key.
		In:      run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: util.MaskMarker},
		WantKey: "PD",
	}}
	for testNum, test := range tests {
		t.Run(strconv.Itoa(testNum), func(t *testing.T) {
			t.Parallel()
			got, err := restoreMaskedNotifications([]run.NotifyTarget{test.In}, stored)
			if test.WantErr {
				if err == nil {
					t.Fatalf("restore = %+v, want a refusal", got)
				}
				return
			}
			if err != nil || len(got) != 1 || got[0].Key != test.WantKey {
				t.Fatalf("restore = %+v, %v; want one row with key %q", got, err, test.WantKey)
			}
		})
	}
}
