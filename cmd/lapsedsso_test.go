package cmd

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
)

// TestALapsedLicenseFallsBackRatherThanRefusingToStart covers the promise the pricing page makes in
// its own words: "A lapsed license takes nothing: sign-in falls back to local accounts, and your
// data, evidence, and every Community feature keep working."
//
// The server did the opposite. Directory sign-in configured plus a license that had expired made
// serve return the licensing error and exit, so the renewal slipping by a day took a working
// install offline, on the exact tier whose selling point is that lapsing costs nothing. Falling
// back is not a loosening: local accounts are how every Community install authenticates.
//
// An ABSENT license is left alone. An install that never had single sign-on and configured it
// anyway is a misconfiguration worth one line at startup, not a setting silently ignored.
func TestALapsedLicenseFallsBackRatherThanRefusingToStart(t *testing.T) {
	original := serveOIDCIssuer
	t.Cleanup(func() { serveOIDCIssuer = original; license.Set(nil) })

	// A license that expired yesterday, for the tier that sells directory sign-in.
	lapsed := &license.License{}
	lapsed.Claims.Tier = "pro"
	lapsed.Claims.Org = "acme"
	lapsed.Claims.Expires = time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	if !lapsed.Expired(time.Now()) {
		t.Fatal("the fixture is not actually expired, so this test proves nothing")
	}

	license.Set(lapsed)
	serveOIDCIssuer = "https://idp.example.com"
	if err := license.Allow(license.FeatureSSO); err == nil {
		t.Fatal("a lapsed license should not satisfy the SSO gate, or the fallback never runs")
	}
	if !externalAuthConfigured() {
		t.Fatal("the fixture did not configure directory sign-in")
	}

	disableExternalAuth()
	if externalAuthConfigured() {
		t.Error("directory sign-in survived the fallback, so the server would still refuse to start")
	}
}
