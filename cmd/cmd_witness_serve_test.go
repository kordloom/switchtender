package cmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/util"
	"github.com/kordloom/switchtender/witness"
)

// writeAttestation signs an attestation with a fresh identity and writes it, returning the path and
// both published names for the signer's key: the raw hex key the attestation carries and the sha256
// key id the witness prints and tells operators to publish.
func writeAttestation(t *testing.T,
	mutate func(*witness.Attestation)) (path, signer, keyID string) {
	t.Helper()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	a := &witness.Attestation{
		Server: "https://st.example", MintedAt: time.Now().UTC(),
		CheckpointAt: time.Now().UTC(), LastBeat: 7, LastSeq: 70, LastHead: "aa",
		BeatsRemembered: 7, FindingsTotal: 2,
	}
	if err := witness.SignAttestation(a, id); err != nil {
		t.Fatalf("SignAttestation() error = %v", err)
	}
	if mutate != nil {
		mutate(a)
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	path = filepath.Join(t.TempDir(), "attestation.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path, id.PublicKeyHex(), id.KeyID()
}

func TestWitnessVerifyAttestation(t *testing.T) {
	good, signer, keyID := writeAttestation(t, nil)
	tampered, _, _ := writeAttestation(t, func(a *witness.Attestation) { a.FindingsTotal = 0 })

	tests := []struct {
		// Name says what the case proves.
		Name string
		// Path is the attestation argument.
		Path string
		// Pin is the --pubkey value.
		Pin string
		// WantErr is whether verification must fail.
		WantErr bool
	}{{ // Test 0: A signed attestation with the right pin verifies.
		Name: "right pin", Path: good, Pin: signer,
	}, { // Test 1: The key id is the form the witness prints and the help says to publish, so a
		// relying party who pinned exactly what they were told to pin must verify. It was compared
		// against the raw hex key alone, so that relying party got a refusal on a sound attestation
		// and the documented path to check a witness without trusting the operator always failed.
		Name: "key id pin", Path: good, Pin: keyID,
	}, { // Test 1: Without a pin the document is only checked for internal consistency.
		Name: "no pin", Path: good,
	}, { // Test 2: The pin is the trust decision; a different key is refused even when the
		// signature itself is sound.
		Name: "wrong pin", Path: good, Pin: "deadbeef", WantErr: true,
	}, { // Test 3: An altered field breaks the signature.
		Name: "tampered", Path: tampered, WantErr: true,
	}, { // Test 4: A missing file is an error, not a pass.
		Name: "missing", Path: filepath.Join(t.TempDir(), "nope.json"), WantErr: true,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			witnessVerifyPubkey = test.Pin
			defer func() { witnessVerifyPubkey = "" }()
			err := runWitnessVerify(testCommand(), []string{test.Path})
			if (err != nil) != test.WantErr {
				t.Errorf("runWitnessVerify() error = %v, want error %v", err, test.WantErr)
			}
		})
	}
}

func TestWitnessServeRefusesBadFlags(t *testing.T) {
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Watch is the --watch list.
		Watch []string
		// Interval is the --interval value.
		Interval time.Duration
	}{{ // Test 0: No servers to watch is a misconfiguration, not an idle witness.
		Name: "no watch", Interval: time.Minute,
	}, { // Test 1: A watch target must be a web URL.
		Name: "bad scheme", Watch: []string{"ftp://st.example"}, Interval: time.Minute,
	}, { // Test 2: An interval under the floor would hammer the watched servers.
		Name: "short interval", Watch: []string{"https://st.example"}, Interval: time.Second,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			witnessWatch, witnessServeInterval = test.Watch, test.Interval
			defer func() { witnessWatch, witnessServeInterval = nil, time.Minute }()
			if err := runWitnessServe(testCommand(), nil); err == nil {
				t.Error("runWitnessServe() = nil error, want the misconfiguration refused")
			}
		})
	}
}

func TestPostWitnessFindingTreatsRefusalAsFailure(t *testing.T) {
	t.Parallel()
	var status int32 = 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&status)))
	}))
	defer srv.Close()
	f := witness.Finding{Kind: "head_regression", Detail: "d"}
	if err := postWitnessFinding(context.Background(), srv.Client(), srv.URL, "s", f); err == nil {
		t.Error("postWitnessFinding() = nil error on a 500; a refused delivery did not happen")
	}
	atomic.StoreInt32(&status, 204)
	if err := postWitnessFinding(context.Background(), srv.Client(), srv.URL, "s", f); err != nil {
		t.Errorf("postWitnessFinding() error = %v on a 204", err)
	}
}

// TestPostWitnessFindingCarriesTheClass pins that the webhook payload names a finding's class when
// the finding has one, so a channel can route a refusal apart from a note, and leaves the key out
// when it has none, so a pull server's payload keeps its shape.
func TestPostWitnessFindingCarriesTheClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Finding   witness.Finding
		WantClass string
		WantKey   bool
	}{{ // Test 0: A refusal about an enrolled install carries its class.
		Name:      "refusal",
		Finding:   witness.Finding{Kind: "rewritten_beat", Detail: "d", Class: witness.ClassRefusal},
		WantClass: witness.ClassRefusal, WantKey: true,
	}, { // Test 1: A note about the witness's coverage carries its class.
		Name:      "note",
		Finding:   witness.Finding{Kind: "silent_install", Detail: "d", Class: witness.ClassNote},
		WantClass: witness.ClassNote, WantKey: true,
	}, { // Test 2: A finding about a watched server has no class, so the key is omitted.
		Name:    "pull server",
		Finding: witness.Finding{Kind: "head_regression", Detail: "d"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			bodies := make(chan map[string]any, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode webhook body: %v", err)
				}
				bodies <- body
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			err := postWitnessFinding(context.Background(), srv.Client(), srv.URL, "s", test.Finding)
			if err != nil {
				t.Fatalf("postWitnessFinding() error = %v", err)
			}
			got := <-bodies
			class, has := got["class"]
			if has != test.WantKey {
				t.Fatalf("payload has class = %v, want %v: %v", has, test.WantKey, got)
			}
			if has && class != test.WantClass {
				t.Errorf("payload class = %v, want %q", class, test.WantClass)
			}
			if got["kind"] != test.Finding.Kind {
				t.Errorf("payload kind = %v, want %q", got["kind"], test.Finding.Kind)
			}
		})
	}
}

func TestDeliverFindingsRetriesUntilTheChannelTakesThem(t *testing.T) {
	var accepted []string
	var refuse atomic.Bool
	refuse.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var body struct {
			Kind string `json:"kind"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		accepted = append(accepted, body.Kind)
	}))
	defer srv.Close()
	witnessWebhook = srv.URL
	defer func() { witnessWebhook = "" }()

	queue := []witness.Finding{{Kind: "a"}, {Kind: "b"}}
	// The channel is down: everything stays queued instead of being dropped.
	queue = deliverFindings(context.Background(), srv.Client(), "s", queue)
	if len(queue) != 2 {
		t.Fatalf("undelivered = %d, want both retained while the webhook refuses", len(queue))
	}
	// The channel recovers: the queue drains in order.
	refuse.Store(false)
	queue = deliverFindings(context.Background(), srv.Client(), "s", queue)
	if len(queue) != 0 || len(accepted) != 2 || accepted[0] != "a" || accepted[1] != "b" {
		t.Errorf("after recovery queue = %d accepted = %v, want both delivered in order", len(queue), accepted)
	}
}

// TestListenIsLoopback pins the boundary the open-witness refusal turns on. A wildcard or empty
// host binds every interface, so calling it loopback would let a public witness serve its watched
// server list to anyone; a hostname other than localhost cannot be trusted to resolve locally.
func TestListenIsLoopback(t *testing.T) {
	tests := []struct {
		Addr string
		Want bool
	}{{ // Test 0: The IPv4 loopback is loopback.
		Addr: "127.0.0.1:9440", Want: true,
	}, { // Test 1: Any address in the loopback block counts.
		Addr: "127.0.0.2:9440", Want: true,
	}, { // Test 2: The IPv6 loopback is loopback.
		Addr: "[::1]:9440", Want: true,
	}, { // Test 3: localhost is loopback.
		Addr: "localhost:9440", Want: true,
	}, { // Test 4: The IPv4 wildcard binds every interface.
		Addr: "0.0.0.0:9440", Want: false,
	}, { // Test 5: The IPv6 wildcard binds every interface.
		Addr: "[::]:9440", Want: false,
	}, { // Test 6: An empty host binds every interface.
		Addr: ":9440", Want: false,
	}, { // Test 7: A private address is still not loopback.
		Addr: "10.0.0.5:9440", Want: false,
	}, { // Test 8: A hostname other than localhost is not trusted to be local.
		Addr: "witness.example.com:9440", Want: false,
	}, { // Test 9: A bare loopback address with no port still reads as loopback.
		Addr: "127.0.0.1", Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if got := listenIsLoopback(test.Addr); got != test.Want {
				t.Errorf("listenIsLoopback(%q) = %v, want %v", test.Addr, got, test.Want)
			}
		})
	}
}

// TestWitnessServeRefusesOpenOnNonLoopback proves the serve command will not expose the watched
// server list to the network: a non-loopback bind with no read token is refused before anything
// starts, and setting the token through the environment satisfies the check.
func TestWitnessServeRefusesOpenOnNonLoopback(t *testing.T) {
	witnessWatch, witnessServeInterval = []string{"https://st.example"}, time.Minute
	witnessListen = "0.0.0.0:0"
	defer func() {
		witnessWatch, witnessServeInterval = nil, time.Minute
		witnessListen = "127.0.0.1:9440"
		witnessServeToken = ""
	}()

	// No token anywhere: refused, and the error says how to fix it.
	t.Setenv(witnessTokenEnv, "")
	err := runWitnessServe(testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), witnessTokenEnv) {
		t.Fatalf("runWitnessServe() open on 0.0.0.0 = %v, want a refusal naming %s", err, witnessTokenEnv)
	}

	// A token from the environment passes the exposure check. The state directory is forced under
	// a file so the run then fails fast at the key directory, proving the refusal above was the
	// token check and not a later failure.
	t.Setenv(witnessTokenEnv, "read-token")
	file := filepath.Join(t.TempDir(), "occupied")
	if werr := os.WriteFile(file, []byte("x"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	witnessStateDir, witnessServeKeyDir = filepath.Join(file, "state"), ""
	defer func() { witnessStateDir = "switchtender-witness" }()
	err = runWitnessServe(testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), "witness key directory") {
		t.Fatalf("runWitnessServe() with env token = %v, want it past the exposure check and "+
			"failing at the key directory", err)
	}
}

// policyNow is the time the enrollment policy tests read.
var policyNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// policyLicenseKey registers a license signing key for the policy tests and returns its kid and
// private key.
func policyLicenseKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	kid := fmt.Sprintf("witness-policy-%x", pub[:6])
	license.RegisterKey(kid, pub)
	return kid, priv
}

// signedLicense returns a license file for the given tier, add-ons, id, and term, signed by priv
// under kid.
func signedLicense(t *testing.T, kid string, priv ed25519.PrivateKey, id, tier string,
	addons []string, expires time.Time) []byte {
	t.Helper()
	raw, err := license.Sign(license.Claims{V: 1, ID: id, Org: "Acme Witness Org", Tier: tier,
		Hosts: "250", Issued: "2026-01-01T00:00:00Z", Expires: expires.Format(time.RFC3339), Kid: kid,
		Addons: addons}, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	return raw
}

// TestEnrollmentPolicyFollowsTheLicense is invariant 5: under the licensed policy an install
// enrolls only with a license that verifies against the compiled-in keys, includes the hosted
// witness, and is within its term, and the policy hands the witness the license id and term alone.
// Every row's coverage is also held to the license package's own gate, so the policy's restatement
// of the coverage rule cannot drift from the rule.
func TestEnrollmentPolicyFollowsTheLicense(t *testing.T) { //nolint:funlen // Test function.
	// Not parallel: the coverage cross-check installs each license as the process's own.
	previous := license.Current()
	t.Cleanup(func() { license.Set(previous) })
	kid, priv := policyLicenseKey(t)
	_, stranger := policyLicenseKey(t)
	year := policyNow.Add(365 * 24 * time.Hour)
	addon := []string{license.AddonHostedWitness}
	tampered := signedLicense(t, kid, priv, "lic_tampered", license.TierTeam, nil, year)
	tampered = bytes.Replace(tampered, []byte(`"tier": "team"`),
		[]byte(`"tier": "team", "addons": ["hosted-witness"]`), 1)
	if !bytes.Contains(tampered, []byte(license.AddonHostedWitness)) {
		t.Fatalf("the tampered license was not edited:\n%s", tampered)
	}
	tests := []struct {
		// Name says what the row proves.
		Name string
		// License is the license file the enrollment carries, nil for none.
		License []byte
		// WantID is the license id an entitled row is enrolled under, empty when refused.
		WantID string
	}{{ // Test 0: Community carries no license.
		Name: "community",
	}, { // Test 1: Pro does not include the hosted witness.
		Name: "pro", License: signedLicense(t, kid, priv, "lic_pro", license.TierPro, nil, year),
	}, { // Test 2: Team does not include it without the add-on.
		Name: "team", License: signedLicense(t, kid, priv, "lic_team", license.TierTeam, nil, year),
	}, { // Test 3: Team with the add-on enrolls.
		Name:    "team with the add-on",
		License: signedLicense(t, kid, priv, "lic_team_addon", license.TierTeam, addon, year),
		WantID:  "lic_team_addon",
	}, { // Test 4: Enterprise includes it.
		Name:    "enterprise",
		License: signedLicense(t, kid, priv, "lic_ent", license.TierEnterprise, nil, year),
		WantID:  "lic_ent",
	}, { // Test 5: A license minted before tiers below Team is Team, and the add-on counts.
		Name:    "legacy team with the add-on",
		License: signedLicense(t, kid, priv, "lic_legacy", "", addon, year), WantID: "lic_legacy",
	}, { // Test 6: The add-on is not sold on Pro.
		Name:    "pro with the add-on",
		License: signedLicense(t, kid, priv, "lic_pro_addon", license.TierPro, addon, year),
	}, { // Test 7: A tier this build does not know covers nothing.
		Name:    "unknown tier",
		License: signedLicense(t, kid, priv, "lic_odd", "platinum", addon, year),
	}, { // Test 8: An Enterprise license whose term has ended.
		Name: "expired",
		License: signedLicense(t, kid, priv, "lic_expired", license.TierEnterprise, nil,
			policyNow.Add(-time.Hour)),
	}, { // Test 9: Signed by a key this build does not trust, under a trusted kid.
		Name: "forged",
		License: signedLicense(t, kid, stranger, "lic_forged", license.TierEnterprise, nil,
			year),
	}, { // Test 10: A Team license with the add-on written in after signing.
		Name: "tampered", License: tampered,
	}, { // Test 11: A license with no id, whose installs cannot be counted.
		Name:    "no id",
		License: signedLicense(t, kid, priv, "", license.TierEnterprise, nil, year),
	}, { // Test 12: Something that is not a license file at all.
		Name: "not a license", License: []byte(`{"claims":{"v":1},"sig":"AA=="}`),
	}}
	policy := licensedEnrollment(func() time.Time { return policyNow })
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			ent, err := policy(witness.EnrollRequest{License: test.License})
			if (err == nil) != (test.WantID != "") || ent.LicenseID != test.WantID {
				t.Fatalf("policy() = %+v, %v, want license %q", ent, err, test.WantID)
			}
			if err == nil && !ent.Expires.Equal(year) {
				t.Errorf("policy() term = %s, want %s", ent.Expires, year)
			}
			l, verr := license.Verify(test.License, "")
			if verr != nil {
				return
			}
			license.Set(l)
			gate := license.Allow(license.FeatureHostedWitness)
			license.Set(previous)
			if covered := gate == nil || errors.Is(gate, license.ErrLapsed); covered !=
				l.Covers(license.FeatureHostedWitness) {
				t.Errorf("the policy says covered = %v and the license gate says %v (%v)",
					l.Covers(license.FeatureHostedWitness), covered, gate)
			}
		})
	}
}

// TestWitnessServeEnrollsUnderTheLicensedPolicy runs the licensed policy inside a witness service
// over HTTP: an install carrying a Team license with the add-on enrolls and gets an answer signed
// by the witness, and one carrying a Pro license is refused with a 403.
func TestWitnessServeEnrollsUnderTheLicensedPolicy(t *testing.T) {
	t.Parallel()
	kid, priv := policyLicenseKey(t)
	year := time.Now().Add(365 * 24 * time.Hour)
	id, err := audit.LoadWitnessIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadWitnessIdentity() error = %v", err)
	}
	svc, err := witness.NewService(id, t.TempDir(), time.Minute, nil, nil, nil,
		witness.WithAcceptEnrollments(witness.DefaultMaxInstalls),
		witness.WithEnrollmentPolicy(licensedEnrollment(time.Now)),
		witness.WithWitnessKeys([]witness.WitnessKey{witness.WitnessKeyOf(id, time.Now())}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	api := httptest.NewServer(svc.Handler())
	t.Cleanup(api.Close)
	client := api.Client()
	tests := []struct {
		// License is the license file the install carries.
		License []byte
		// WantCode is the answer.
		WantCode int
	}{{ // Test 0: Team with the add-on.
		License: signedLicense(t, kid, priv, "lic_serve", license.TierTeam,
			[]string{license.AddonHostedWitness}, year),
		WantCode: http.StatusOK,
	}, { // Test 1: Pro.
		License:  signedLicense(t, kid, priv, "lic_serve_pro", license.TierPro, nil, year),
		WantCode: http.StatusForbidden,
	}}
	for testNum, test := range tests {
		producer, err := audit.LoadIdentity(t.TempDir())
		if err != nil {
			t.Fatalf("LoadIdentity() error = %v", err)
		}
		req := witness.EnrollRequest{InstallID: producer.InstallID, License: test.License,
			At: time.Now().UTC().Format(time.RFC3339)}
		if err := witness.SignEnroll(&req, producer); err != nil {
			t.Fatalf("SignEnroll() error = %v", err)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		res, err := client.Post(api.URL+"/v1/enroll", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("test %d: POST error = %v", testNum, err)
		}
		var resp witness.EnrollResponse
		derr := json.NewDecoder(res.Body).Decode(&resp)
		// Read to the end so the connection is reused and closed by the server, not by the client.
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != test.WantCode {
			t.Errorf("test %d: status = %d, want %d", testNum, res.StatusCode, test.WantCode)
			continue
		}
		if test.WantCode != http.StatusOK {
			continue
		}
		if derr != nil {
			t.Fatalf("test %d: decode answer: %v", testNum, derr)
		}
		if signer, err := witness.VerifyEnrollResponse(&resp); err != nil || signer != id.KeyID() ||
			resp.KeyID != producer.KeyID() {
			t.Errorf("test %d: answer verify = %s, %v, key %s; want signed by %s for %s", testNum,
				signer, err, resp.KeyID, id.KeyID(), producer.KeyID())
		}
	}
}

// TestWitnessServeRefusesBadEnrollmentFlags pins the enrollment flags: an unknown policy and a cap
// under one are refused before anything starts, and --accept-enrollments makes --watch optional.
func TestWitnessServeRefusesBadEnrollmentFlags(t *testing.T) {
	t.Cleanup(func() {
		witnessWatch, witnessServeInterval = nil, time.Minute
		witnessAcceptEnrollments, witnessEnrollmentPolicy = false, policyLicensed
		witnessMaxInstalls = witness.DefaultMaxInstalls
		witnessStateDir, witnessServeKeyDir = "switchtender-witness", ""
	})
	file := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		// Name says what the row proves.
		Name string
		// Watch is the --watch list.
		Watch []string
		// Accept is --accept-enrollments.
		Accept bool
		// Policy is --enrollment-policy.
		Policy string
		// Max is --max-installs-per-license.
		Max int
		// WantErr is a fragment the refusal must contain.
		WantErr string
	}{{ // Test 0: A policy that does not exist.
		Name: "unknown policy", Watch: []string{"https://st.example"}, Policy: "anyone",
		Max: 10, WantErr: "--enrollment-policy",
	}, { // Test 1: A cap of no installs at all.
		Name: "zero cap", Watch: []string{"https://st.example"}, Policy: policyOpen, Max: 0,
		WantErr: "--max-installs-per-license",
	}, { // Test 2: Enrolling with nothing to watch gets past the watch check, and fails at the
		// key directory forced under a file.
		Name: "enroll without watch", Accept: true, Policy: policyLicensed, Max: 10,
		WantErr: "witness key directory",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			witnessWatch, witnessServeInterval = test.Watch, time.Minute
			witnessAcceptEnrollments, witnessEnrollmentPolicy = test.Accept, test.Policy
			witnessMaxInstalls = test.Max
			witnessStateDir, witnessServeKeyDir = filepath.Join(file, "state"), ""
			err := runWitnessServe(testCommand(), nil)
			if err == nil || !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("runWitnessServe() = %v, want a refusal naming %q", err, test.WantErr)
			}
		})
	}
}

// TestEnrollmentPolicyNamesOnlyItsTwoPolicies pins the names --enrollment-policy accepts.
func TestEnrollmentPolicyNamesOnlyItsTwoPolicies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name is the flag value.
		Name string
		// WantErr is whether it is refused.
		WantErr bool
	}{{ // Test 0: The hosted witness's policy.
		Name: policyLicensed,
	}, { // Test 1: The private push witness's policy.
		Name: policyOpen,
	}, { // Test 2: Anything else.
		Name: "Licensed", WantErr: true,
	}, { // Test 3: Nothing.
		Name: "", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p, err := enrollmentPolicy(test.Name, time.Now)
			if (err != nil) != test.WantErr || (p == nil) != test.WantErr {
				t.Errorf("enrollmentPolicy(%q) = %v, %v, want error %v", test.Name, p != nil, err,
					test.WantErr)
			}
		})
	}
}

// TestWitnessSlackTextNamesTheFinding pins what a Slack message about a finding says: its kind, its
// class, the producer key id of the install it is about, and its detail. A finding about a watched
// server has no class and names the server instead.
func TestWitnessSlackTextNamesTheFinding(t *testing.T) {
	t.Parallel()
	const keyID = "sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Subject is what the service names the finding's subject by.
		Subject string
		// Finding is the finding posted.
		Finding witness.Finding
		// WantParts are the strings the message must carry.
		WantParts []string
	}{{ // Test 0: A refusal names its class, kind, producer key, and detail.
		Name: "refusal", Subject: keyID,
		Finding: witness.Finding{Kind: "rewritten_history", Detail: "the tree at size 9 differs",
			Class: witness.ClassRefusal},
		WantParts: []string{"refusal", "rewritten_history", "producer key " + keyID,
			"the tree at size 9 differs"},
	}, { // Test 1: A note names its class, kind, producer key, and detail.
		Name: "note", Subject: keyID,
		Finding: witness.Finding{Kind: "silent_install", Detail: "nothing pushed since noon",
			Class: witness.ClassNote},
		WantParts: []string{"note", "silent_install", "producer key " + keyID,
			"nothing pushed since noon"},
	}, { // Test 2: A finding about a watched server names the server.
		Name: "watched server", Subject: "https://st.example",
		Finding:   witness.Finding{Kind: "head_regression", Detail: "the head went back"},
		WantParts: []string{"head_regression", "watched server https://st.example", "the head went back"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := witnessSlackText(test.Subject, test.Finding)
			for _, part := range test.WantParts {
				if !strings.Contains(got, part) {
					t.Errorf("witnessSlackText() = %q, missing %q", got, part)
				}
			}
		})
	}
}

// TestPostWitnessSlackSendsText pins the body Slack requires: a JSON object whose text member is
// the message. Slack refuses an incoming webhook body without it.
func TestPostWitnessSlackSendsText(t *testing.T) {
	t.Parallel()
	type received struct {
		// Body is the decoded request body.
		Body map[string]any
		// ContentType is the request's content type.
		ContentType string
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Slack body: %v", err)
		}
		got <- received{Body: body, ContentType: r.Header.Get("Content-Type")}
	}))
	defer srv.Close()
	f := witness.Finding{Kind: "first_root", Detail: "memory starts at size 3",
		Class: witness.ClassNote}
	want := witnessSlackText("sha256:ab", f)
	if err := postWitnessSlack(context.Background(), srv.Client(), srv.URL, want,
		time.Millisecond); err != nil {
		t.Fatalf("postWitnessSlack() error = %v", err)
	}
	r := <-got
	text, ok := r.Body["text"]
	if !ok {
		t.Fatalf("Slack body = %v, has no text member", r.Body)
	}
	if text != want {
		t.Errorf("Slack body text = %q, want %q", text, want)
	}
	if r.ContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", r.ContentType)
	}
}

// TestPostWitnessSlackRetriesOnce pins the retry: a failed post is tried exactly once more, so a
// passing fault at Slack costs no alert and a channel that keeps failing is not hammered.
func TestPostWitnessSlackRetriesOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Failures is how many posts the channel answers 500 before it answers 200.
		Failures int32
		// WantCalls is how many posts reach the channel.
		WantCalls int32
		// WantErr reports whether the delivery fails.
		WantErr bool
	}{{ // Test 0: A channel that takes the first post is asked once.
		Name: "first post taken", Failures: 0, WantCalls: 1,
	}, { // Test 1: One 5xx, then the retry is taken.
		Name: "one 5xx then success", Failures: 1, WantCalls: 2,
	}, { // Test 2: A channel that keeps failing is asked twice and the delivery fails.
		Name: "keeps failing", Failures: 100, WantCalls: 2, WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) <= test.Failures {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			err := postWitnessSlack(context.Background(), srv.Client(), srv.URL, "m", time.Millisecond)
			if (err != nil) != test.WantErr {
				t.Errorf("postWitnessSlack() error = %v, want error %v", err, test.WantErr)
			}
			if got := calls.Load(); got != test.WantCalls {
				t.Errorf("channel saw %d posts, want %d", got, test.WantCalls)
			}
		})
	}
}

// TestPostWitnessSlackMasksTheURL pins that a failed delivery's error never carries the webhook
// URL's path, which is the credential. net/http writes the whole URL into a transport error.
func TestPostWitnessSlackMasksTheURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	webhook := srv.URL + "/services/T000/B000/secret-token-path"
	srv.Close()
	err := postWitnessSlack(context.Background(), &http.Client{Timeout: time.Second}, webhook, "m",
		time.Millisecond)
	if err == nil {
		t.Fatal("postWitnessSlack() = nil error against a closed server")
	}
	if strings.Contains(err.Error(), "secret-token-path") {
		t.Errorf("postWitnessSlack() error = %q, carries the webhook's secret path", err)
	}
}

// TestWitnessSlackURLsFallBackToTheEnvironment pins where the Slack URLs come from: the flag when
// it is given, else the environment variable, and that a value that is not a URL is refused
// without being quoted.
func TestWitnessSlackURLsFallBackToTheEnvironment(t *testing.T) {
	t.Parallel()
	const secret = "https://hooks.slack.example/services/T/B/secret"
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Flagged are the --notify-slack values.
		Flagged []string
		// Env is the environment variable's value.
		Env string
		// WantURLs are the URLs findings go to.
		WantURLs []string
		// WantErr reports whether the configuration is refused.
		WantErr bool
	}{{ // Test 0: Nothing configured means no Slack.
		Name: "neither",
	}, { // Test 1: The environment supplies the URL when the flag is absent.
		Name: "environment", Env: secret, WantURLs: []string{secret},
	}, { // Test 2: Surrounding space in the environment value is ignored.
		Name: "environment trimmed", Env: "  " + secret + "\n", WantURLs: []string{secret},
	}, { // Test 3: The flag wins over the environment.
		Name: "flag wins", Flagged: []string{"https://a.example/x", "https://b.example/y"},
		Env: secret, WantURLs: []string{"https://a.example/x", "https://b.example/y"},
	}, { // Test 4: A flag value that is not a URL is refused.
		Name: "bad flag", Flagged: []string{"hooks.slack.example/secret"}, WantErr: true,
	}, { // Test 5: An environment value that is not a URL is refused.
		Name: "bad environment", Env: "ftp://hooks.slack.example/secret", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			getenv := func(name string) string {
				if name == witnessSlackEnv {
					return test.Env
				}
				return ""
			}
			got, err := witnessSlackURLs(test.Flagged, getenv)
			if (err != nil) != test.WantErr {
				t.Fatalf("witnessSlackURLs() error = %v, want error %v", err, test.WantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("witnessSlackURLs() error = %q, quotes the value", err)
			}
			if strings.Join(got, ",") != strings.Join(test.WantURLs, ",") {
				t.Errorf("witnessSlackURLs() = %v, want %v", got, test.WantURLs)
			}
		})
	}
}

// TestWitnessSlackReadsItsEnvironmentVariable pins the variable's name against the real
// environment, since the runbook's env file sets it by that name.
func TestWitnessSlackReadsItsEnvironmentVariable(t *testing.T) {
	t.Setenv("SWITCHTENDER_WITNESS_NOTIFY_SLACK", "https://hooks.slack.example/services/T/B/x")
	got, err := witnessSlackURLs(nil, os.Getenv)
	if err != nil {
		t.Fatalf("witnessSlackURLs() error = %v", err)
	}
	if len(got) != 1 || got[0] != "https://hooks.slack.example/services/T/B/x" {
		t.Errorf("witnessSlackURLs() = %v, want the URL from SWITCHTENDER_WITNESS_NOTIFY_SLACK", got)
	}
}

// TestWitnessSlackDeliversInOrderOffThePushPath pins that queuing a finding never waits on Slack,
// so a push answer is not held by the channel, and that findings arrive in the order raised.
func TestWitnessSlackDeliversInOrderOffThePushPath(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	texts := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-release
		var body struct {
			// Text is the message.
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		texts <- body.Text
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newWitnessSlack([]string{srv.URL}, srv.Client(), time.Millisecond, zap.NewNop())
	go n.run(ctx)
	first := witness.Finding{Kind: "first_root", Detail: "one", Class: witness.ClassNote}
	second := witness.Finding{Kind: "missing_beat", Detail: "two", Class: witness.ClassNote}
	start := time.Now()
	n.notify("sha256:ab", first)
	n.notify("sha256:ab", second)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("notify took %s while Slack was stalled, want it to return at once", took)
	}
	close(release)
	for i, want := range []witness.Finding{first, second} {
		select {
		case got := <-texts:
			if got != witnessSlackText("sha256:ab", want) {
				t.Errorf("message %d = %q, want %q", i, got, witnessSlackText("sha256:ab", want))
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("message %d never arrived", i)
		}
	}
}

// TestWitnessSlackLogsMaskTheURL pins that a delivery that failed twice is logged with the webhook
// URL masked, in every field of the line.
func TestWitnessSlackLogsMaskTheURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	webhook := srv.URL + "/services/T000/B000/secret-token-path"
	core, logs := observer.New(zap.WarnLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newWitnessSlack([]string{webhook}, srv.Client(), time.Millisecond, zap.New(core))
	go n.run(ctx)
	n.notify("sha256:ab", witness.Finding{Kind: "rewritten_history", Detail: "d",
		Class: witness.ClassRefusal})
	deadline := time.Now().Add(10 * time.Second)
	for logs.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	entries := logs.All()
	if len(entries) == 0 {
		t.Fatal("no log line for a delivery that failed twice")
	}
	for _, e := range entries {
		line := e.Message
		for k, v := range e.ContextMap() {
			line += fmt.Sprintf(" %s=%v", k, v)
		}
		if strings.Contains(line, "secret-token-path") {
			t.Errorf("log line carries the webhook's secret path: %s", line)
		}
		if got := e.ContextMap()["url"]; got != util.MaskURL(webhook) {
			t.Errorf("log url = %v, want %q", got, util.MaskURL(webhook))
		}
	}
}

// TestWitnessServeRefusesRetiredKeysInTheStateDirectory pins the serve side of custody: a key
// directory that holds retired keys is refused when it is also the state directory, which is what
// leaving --key-dir unset makes it, before anything is served.
func TestWitnessServeRefusesRetiredKeysInTheStateDirectory(t *testing.T) {
	t.Cleanup(func() {
		witnessWatch, witnessServeInterval = nil, time.Minute
		witnessStateDir, witnessServeKeyDir = "switchtender-witness", ""
	})
	base := t.TempDir()
	keyDir := filepath.Join(base, "keys")
	if _, err := audit.LoadWitnessIdentity(keyDir); err != nil {
		t.Fatalf("LoadWitnessIdentity() error = %v", err)
	}
	if _, err := witness.RotateKey(keyDir, filepath.Join(base, "state"), time.Now()); err != nil {
		t.Fatalf("RotateKey() error = %v", err)
	}
	witnessWatch, witnessServeInterval = []string{"https://st.example"}, time.Minute
	witnessStateDir, witnessServeKeyDir = keyDir, ""
	err := runWitnessServe(testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Errorf("runWitnessServe() = %v, want the rotated key directory refused as the state "+
			"directory", err)
	}
}
