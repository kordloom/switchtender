package run

import "testing"

// TestValidateNotifyTarget covers what each channel needs to be deliverable, since a target that
// would reach no one is refused when saved rather than dropped silently at run time.
func TestValidateNotifyTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Target NotifyTarget
		Want   bool
	}{
		{"slack with url", NotifyTarget{Kind: NotifySlack, URL: "https://h"}, true},
		{"slack without url", NotifyTarget{Kind: NotifySlack}, false},
		{"pagerduty with key", NotifyTarget{Kind: NotifyPagerDuty, Key: "R0"}, true},
		{"pagerduty without key", NotifyTarget{Kind: NotifyPagerDuty}, false},
		{"grafana with url and key", NotifyTarget{Kind: NotifyGrafana, URL: "https://g", Key: "t"}, true},
		{"grafana missing token", NotifyTarget{Kind: NotifyGrafana, URL: "https://g"}, false},
		{"twilio with recipient", NotifyTarget{Kind: NotifyTwilio, To: "+15550100"}, true},
		{"twilio without recipient", NotifyTarget{Kind: NotifyTwilio}, false},
		{"email with recipients", NotifyTarget{Kind: NotifyEmail, To: "a@b.co,c@d.co"}, true},
		{"email without recipients", NotifyTarget{Kind: NotifyEmail}, false},
		{"unknown kind", NotifyTarget{Kind: "carrier-pigeon", URL: "x"}, false},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			err := ValidateNotifyTarget(test.Target)
			if test.Want && err != nil {
				t.Errorf("rejected a deliverable target: %v", err)
			}
			if !test.Want && err == nil {
				t.Error("accepted an undeliverable target")
			}
		})
	}
	// Every kind ValidNotifyKind accepts is one ValidateNotifyTarget can rule on.
	for _, k := range []string{NotifyWebhook, NotifySlack, NotifyMattermost, NotifyRocketChat,
		NotifyDiscord, NotifyTeams, NotifyNtfy, NotifyPagerDuty, NotifyGrafana, NotifyTwilio, NotifyEmail} {
		if !ValidNotifyKind(k) {
			t.Errorf("ValidNotifyKind rejects %q, which is a per-run kind", k)
		}
	}
}
