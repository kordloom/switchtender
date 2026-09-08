package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
)

// quietServeFlags points every serve flag this package reads at a value that starts a plain,
// loopback, feature-free server in a directory of the test's own, so a case can turn on exactly the
// one flag it is about. It also redirects the home directory, since the project checkout cache and
// the identity fall back to per-user paths.
func quietServeFlags(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("SWITCHTENDER_PLUGINS_DIR", "")
	t.Setenv("SWITCHTENDER_WORKER_TOKEN", "")
	t.Setenv("SWITCHTENDER_LICENSE", filepath.Join(home, "no-license.json"))

	setString(t, &serveDB, filepath.Join(t.TempDir(), "switchtender.db"))
	setString(t, &serveAddr, defaultServeAddr)
	setString(t, &policyFile, "")
	setString(t, &serveTLSCert, "")
	setString(t, &serveTLSKey, "")
	setString(t, &servePluginsDir, "")
	setString(t, &serveWorkerPools, "")
	setString(t, &serveWorkerToken, "")
	setString(t, &serveOIDCIssuer, "")
	setString(t, &serveLDAPURL, "")
	setString(t, &serveSAMLIDPMetadataURL, "")
	setString(t, &serveJWTJWKSURL, "")
	setString(t, &serveAIProvider, "")
	setString(t, &forwardURL, "")
	setString(t, &forwardSyslog, "")
	setString(t, &forwardState, filepath.Join(t.TempDir(), "forward.json"))
	setString(t, &evidenceDir, "")
	setString(t, &serveAnchorTSAURL, "")
	setString(t, &retainRuns, "")
	setString(t, &retainEvents, "")
	setBool(t, &serveReadOnly, false)
	setDuration(t, &spanCadence, 0)
	setDuration(t, &evidenceCadence, 0)
	setDuration(t, &forwardInterval, 5*time.Second)

	oldHeaders := forwardHeaders
	t.Cleanup(func() { forwardHeaders = oldHeaders })
	forwardHeaders = nil

	oldListener := serveListener
	t.Cleanup(func() { serveListener = oldListener })
	serveListener = nil
}

// TestServeRefusesAMisconfigurationRatherThanRunWithoutTheFeature drives runServe with one bad flag
// at a time and proves each is refused before the server binds.
//
// Every case here is a feature the operator switched on. The failure mode these guards prevent is
// not a crash, it is silence: a server that starts, logs nothing wrong, and runs with the evidence
// register, the beat emitter, or the audit forwarder quietly off while the command line says they
// are on. A negative evidence cadence did exactly that once, passing the pairing check and then
// falling through the positive guard.
//
// The refusal has to happen before the listener is bound, or an install answers requests while the
// operator is still reading the error.
func TestServeRefusesAMisconfigurationRatherThanRunWithoutTheFeature(t *testing.T) {
	tests := []struct {
		Name     string
		Setup    func(t *testing.T)
		WantWord string
	}{{ // Test 0: A beat cadence under a second cannot be committed into a beat entry.
		Name:     "sub second span cadence",
		Setup:    func(t *testing.T) { setDuration(t, &spanCadence, 500*time.Millisecond) },
		WantWord: "span-cadence",
	}, { // Test 1: A cadence that is not a whole number of seconds is refused rather than rounded.
		Name:     "fractional span cadence",
		Setup:    func(t *testing.T) { setDuration(t, &spanCadence, 1500*time.Millisecond) },
		WantWord: "span-cadence",
	}, { // Test 2: A negative cadence is refused rather than read as beats off.
		Name:     "negative span cadence",
		Setup:    func(t *testing.T) { setDuration(t, &spanCadence, -time.Second) },
		WantWord: "span-cadence",
	}, { // Test 3: A negative evidence cadence is refused; it used to fall through and leave the
		// change register silently off while the flags said it was on.
		Name: "negative evidence cadence",
		Setup: func(t *testing.T) {
			setString(t, &evidenceDir, t.TempDir())
			setDuration(t, &evidenceCadence, -time.Hour)
		},
		WantWord: "evidence-cadence must be positive",
	}, { // Test 4: A directory with no cadence writes nothing, so it is refused.
		Name: "evidence dir without cadence",
		Setup: func(t *testing.T) {
			setString(t, &evidenceDir, t.TempDir())
			setDuration(t, &evidenceCadence, 0)
		},
		WantWord: "set together",
	}, { // Test 5: A cadence with no directory has nowhere to write, so it is refused.
		Name: "evidence cadence without dir",
		Setup: func(t *testing.T) {
			setString(t, &evidenceDir, "")
			setDuration(t, &evidenceCadence, 2160*time.Hour)
		},
		WantWord: "set together",
	}, { // Test 6: A cadence under the floor would write registers faster than a review reads them.
		Name: "evidence cadence too short",
		Setup: func(t *testing.T) {
			setString(t, &evidenceDir, t.TempDir())
			setDuration(t, &evidenceCadence, time.Minute)
		},
		WantWord: "at least 1h",
	}, { // Test 7: A forward target that is not a web URL is refused.
		Name:     "forward url bad scheme",
		Setup:    func(t *testing.T) { setString(t, &forwardURL, "ftp://siem.example/events") },
		WantWord: "http or https",
	}, { // Test 8: A forward target with no host names nothing.
		Name:     "forward url no host",
		Setup:    func(t *testing.T) { setString(t, &forwardURL, "https://") },
		WantWord: "http or https",
	}, { // Test 9: A malformed header would be dropped silently, so it is refused.
		Name: "forward header without a colon",
		Setup: func(t *testing.T) {
			setString(t, &forwardURL, "https://siem.example/events")
			forwardHeaders = []string{"Authorization Bearer abc"}
		},
		WantWord: "Name: value",
	}, { // Test 10: A header with an empty name is refused.
		Name: "forward header with an empty name",
		Setup: func(t *testing.T) {
			setString(t, &forwardURL, "https://siem.example/events")
			forwardHeaders = []string{"  : value"}
		},
		WantWord: "Name: value",
	}, { // Test 11: A syslog collector has to be host:port.
		Name:     "forward syslog without a port",
		Setup:    func(t *testing.T) { setString(t, &forwardSyslog, "collector.example") },
		WantWord: "host:port",
	}, { // Test 12: A poll interval under a second would hammer the chain.
		Name: "forward interval too short",
		Setup: func(t *testing.T) {
			setString(t, &forwardURL, "https://siem.example/events")
			setDuration(t, &forwardInterval, 100*time.Millisecond)
		},
		WantWord: "at least 1s",
	}, { // Test 13: A certificate with no key cannot serve HTTPS, and starting on plain HTTP
		// instead would put an install the operator believes is encrypted on the wire in the clear.
		Name:     "tls cert without key",
		Setup:    func(t *testing.T) { setString(t, &serveTLSCert, "/etc/ssl/st.crt") },
		WantWord: "both --tls-cert and --tls-key",
	}, { // Test 14: A key with no certificate is the same misconfiguration.
		Name:     "tls key without cert",
		Setup:    func(t *testing.T) { setString(t, &serveTLSKey, "/etc/ssl/st.key") },
		WantWord: "both --tls-cert and --tls-key",
	}, { // Test 15: A policy file that does not parse stops the server; degrading to no policies
		// would turn a typo into an install where nothing is gated and nothing says so.
		Name: "malformed policy file",
		Setup: func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policies.yaml")
			if err := os.WriteFile(path, []byte("policies: [ unclosed\n  - broken"),
				0o600); err != nil {
				t.Fatalf("write policy file: %v", err)
			}
			setString(t, &policyFile, path)
		},
		WantWord: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: runServe reads package-level flag variables and the environment.
			quietServeFlags(t)
			test.Setup(t)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := testCommand()
			cmd.SetContext(ctx)

			err := runServe(cmd, nil)
			if err == nil {
				t.Fatalf("%s: runServe() = nil error; the server started with the feature the "+
					"flags asked for silently off, or bound with a misconfiguration in place",
					test.Name)
			}
			if test.WantWord != "" && !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("%s: runServe() error = %v, want it to name %q", test.Name, err, test.WantWord)
			}
		})
	}
}

// TestServeRefusesSingleSignOnWithoutALicense pins the one startup gate that is a refusal rather
// than a warning. Single sign-on is configured explicitly by flag, so a missing license there is a
// misconfiguration worth refusing at startup: starting anyway would leave the install with a
// directory nobody can sign in through and an API whose enforcement was derived from empty tables.
func TestServeRefusesSingleSignOnWithoutALicense(t *testing.T) {
	if license.Current() != nil {
		t.Skip("this process already carries a license, so the Community gate cannot be observed")
	}
	tests := []struct {
		Name  string
		Setup func(t *testing.T)
	}{{ // Test 0: OIDC.
		Name:  "oidc",
		Setup: func(t *testing.T) { setString(t, &serveOIDCIssuer, "https://issuer.example") },
	}, { // Test 1: LDAP.
		Name:  "ldap",
		Setup: func(t *testing.T) { setString(t, &serveLDAPURL, "ldaps://ldap.example:636") },
	}, { // Test 2: SAML.
		Name: "saml",
		Setup: func(t *testing.T) {
			setString(t, &serveSAMLIDPMetadataURL, "https://idp.example/metadata")
		},
	}, { // Test 3: Bearer JWT.
		Name:  "jwt",
		Setup: func(t *testing.T) { setString(t, &serveJWTJWKSURL, "https://jwt.example/jwks") },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: runServe reads package-level flag variables and the environment.
			quietServeFlags(t)
			test.Setup(t)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := testCommand()
			cmd.SetContext(ctx)

			err := runServe(cmd, nil)
			if err == nil {
				t.Fatalf("%s: runServe() = nil error with single sign-on configured and no license",
					test.Name)
			}
			if !strings.Contains(err.Error(), "Directory sign-in") {
				t.Errorf("%s: runServe() error = %v, want the sign-on gate rather than some later "+
					"failure", test.Name, err)
			}
		})
	}
}

// TestWorkerRefusesWithoutATeamLicense pins the gate on distributed execution. It sits at the very
// top of the command, before the logger and before any store is opened, so an unlicensed worker
// never reaches the point of leasing a run.
func TestWorkerRefusesWithoutATeamLicense(t *testing.T) {
	if license.Current() != nil {
		t.Skip("this process already carries a license, so the Community gate cannot be observed")
	}
	// Not parallel: the worker reads package-level flag variables and the environment.
	home := t.TempDir()
	t.Setenv("SWITCHTENDER_LICENSE", filepath.Join(home, "no-license.json"))
	setString(t, &workerDB, tempDB(t))
	setString(t, &workerServer, "")

	err := runWorker(testCommand(), nil)
	if err == nil {
		t.Fatal("runWorker() = nil error on Community; distributed workers are gated and the gate " +
			"opened")
	}
	if !strings.Contains(err.Error(), "Distributed workers") {
		t.Errorf("runWorker() error = %v, want the workers gate", err)
	}
}
