package server

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
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
		{Kind: run.NotifyPagerDuty, Key: maskMarker},
		{Kind: run.NotifyTwilio, To: "+15550100"},
		{Kind: run.NotifyEmail, To: "team@example.com"},
	}
	got := restoreMaskedNotifications(incoming, stored)
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
