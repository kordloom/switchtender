package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/witnessclient"
	"github.com/kordloom/switchtender/witness"
)

// enrollWitness is a push witness in open policy that the enroll command tests enroll with.
type enrollWitness struct {
	// id is the witness identity.
	id identity.Identity
	// url is the witness's base URL.
	url string
	// requests counts every request that reached it.
	requests atomic.Int64
}

// newEnrollWitness starts a push witness in open policy and serves it.
func newEnrollWitness(t *testing.T) *enrollWitness {
	t.Helper()
	id, err := audit.LoadWitnessIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadWitnessIdentity() error = %v", err)
	}
	svc, err := witness.NewService(id, t.TempDir(), time.Minute, nil, nil, nil,
		witness.WithAcceptEnrollments(witness.DefaultMaxInstalls),
		witness.WithEnrollmentPolicy(witness.OpenEnrollment),
		witness.WithWitnessKeys([]witness.WitnessKey{
			witness.WitnessKeyOf(id, time.Now().Add(-time.Hour))}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	w := &enrollWitness{id: id}
	h := svc.Handler()
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.requests.Add(1)
		h.ServeHTTP(rw, r)
	}))
	t.Cleanup(api.Close)
	w.url = api.URL
	return w
}

// enrollTestEnv clears the variables that would place the license or the producer key somewhere
// other than beside the test's database, and puts the enroll flags and the process license back
// afterward.
func enrollTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SWITCHTENDER_LICENSE", "")
	t.Setenv(identity.KeyEnv, "")
	t.Setenv(identityDirEnv, "")
	previous := license.Current()
	t.Cleanup(func() {
		license.Set(previous)
		witnessEnrollDB, witnessEnrollURL, witnessEnrollKey = defaultDBPath, "", ""
		witnessEnrollRefresh = false
	})
}

// enrollInstall makes an install directory holding a database and, when lic is not nil, a license
// file beside it, and returns the database path.
func enrollInstall(t *testing.T, lic []byte) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "switchtender.db")
	store, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if lic != nil {
		if err := os.WriteFile(license.PathFor(db), lic, 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	return db
}

// dirDigest returns every file under dir with a digest of its content.
func dirDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		out[strings.TrimPrefix(path, dir)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir() error = %v", err)
	}
	return out
}

// enrollEntries returns the chain entries the enroll command recorded in db's audit chain, as
// method and path.
func enrollEntries(t *testing.T, db string) []string {
	t.Helper()
	store, err := openExisting(db)
	if err != nil {
		t.Fatalf("openExisting() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	chain, err := store.Audits().Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []string
	for _, e := range chain {
		if strings.HasPrefix(e.Path, "/cli/witness/") {
			out = append(out, e.Method+" "+e.Path)
			if e.ContentDigest == "" {
				t.Errorf("the %s entry commits to no summary of the change", e.Path)
			}
		}
	}
	return out
}

// runEnroll runs witness enroll over db with the given flags and returns its output.
func runEnroll(t *testing.T, db, witnessURL, pin string, refresh bool) (string, string, error) {
	t.Helper()
	witnessEnrollDB, witnessEnrollURL, witnessEnrollKey = db, witnessURL, pin
	witnessEnrollRefresh = refresh
	var out, errOut bytes.Buffer
	c := testCommand()
	c.SetOut(&out)
	c.SetErr(&errOut)
	err := runWitnessEnroll(c, nil)
	return out.String(), errOut.String(), err
}

// TestWitnessEnrollRefusesWithoutTheFeatureAndWritesNothing pins the gate: without a license that
// includes the hosted witness the command refuses with the ordinary gate text, sends nothing to the
// witness, and leaves the install directory byte for byte as it was.
func TestWitnessEnrollRefusesWithoutTheFeatureAndWritesNothing(t *testing.T) {
	enrollTestEnv(t)
	w := newEnrollWitness(t)
	kid, priv := policyLicenseKey(t)
	_, stranger := policyLicenseKey(t)
	year := time.Now().Add(365 * 24 * time.Hour)
	tests := []struct {
		// License is the license file beside the database, nil for none.
		License []byte
		// WantErr is a fragment the refusal must contain.
		WantErr string
	}{{ // Test 0: Community has no license file.
		WantErr: "runs Community",
	}, { // Test 1: Pro does not include the hosted witness.
		License: signedLicense(t, kid, priv, "lic_enroll_pro", license.TierPro, nil, year),
		WantErr: "does not include it",
	}, { // Test 2: Team does not include it without the add-on.
		License: signedLicense(t, kid, priv, "lic_enroll_team", license.TierTeam, nil, year),
		WantErr: "does not include it",
	}, { // Test 3: An Enterprise license whose term has ended.
		License: signedLicense(t, kid, priv, "lic_enroll_lapsed", license.TierEnterprise, nil,
			time.Now().Add(-time.Hour)),
		WantErr: "lapsed",
	}, { // Test 4: A license signed by a key this build does not trust.
		License: signedLicense(t, kid, stranger, "lic_enroll_forged", license.TierEnterprise, nil,
			year),
		WantErr: "does not verify",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			db := enrollInstall(t, test.License)
			before := dirDigest(t, filepath.Dir(db))
			_, _, err := runEnroll(t, db, w.url, "", false)
			if err == nil || !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("runWitnessEnroll() = %v, want a refusal naming %q", err, test.WantErr)
			}
			if diff := cmp.Diff(before, dirDigest(t, filepath.Dir(db))); diff != "" {
				t.Errorf("a refused enrollment changed the install directory (-before +after):\n%s",
					diff)
			}
		})
	}
	if got := w.requests.Load(); got != 0 {
		t.Errorf("the witness got %d requests from refused enrollments, want none", got)
	}
}

// TestWitnessEnrollWritesTheEnrollmentBesideTheLicense pins a licensed enrollment: the file lands
// beside the license and binds this install's producer key under the witness's key, the command
// prints the key to compare, enrolling again rewrites the same bytes, a pin is held to the key, the
// refresh mode and another witness are handled without enrolling anew, and an answer signed by a
// key the held list does not accept replaces nothing.
func TestWitnessEnrollWritesTheEnrollmentBesideTheLicense(t *testing.T) { //nolint:funlen // Test function.
	enrollTestEnv(t)
	w := newEnrollWitness(t)
	kid, priv := policyLicenseKey(t)
	lic := signedLicense(t, kid, priv, "lic_enroll_ent", license.TierEnterprise, nil,
		time.Now().Add(365*24*time.Hour))
	db := enrollInstall(t, lic)
	dir := filepath.Dir(db)
	path := filepath.Join(dir, witnessclient.EnrollmentFile)

	out, errOut, err := runEnroll(t, db, w.url, "", false)
	if err != nil {
		t.Fatalf("runWitnessEnroll() error = %v\n%s", err, errOut)
	}
	producer, err := identity.LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	e, err := witnessclient.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if e.Keys.KeyID != w.id.KeyID() || e.KeyID != producer.KeyID() ||
		e.InstallID != producer.InstallID || e.Witness != w.url {
		t.Errorf("enrollment = witness key %s, producer %s, install %s at %s; want %s, %s, %s at %s",
			e.Keys.KeyID, e.KeyID, e.InstallID, e.Witness, w.id.KeyID(), producer.KeyID(),
			producer.InstallID, w.url)
	}
	var printed map[string]any
	if err := json.Unmarshal([]byte(out), &printed); err != nil {
		t.Fatalf("the printed enrollment is not JSON: %v\n%s", err, out)
	}
	if printed["witness_key_id"] != w.id.KeyID() || printed["file"] != path {
		t.Errorf("printed %v, want the witness key %s and the file %s", printed, w.id.KeyID(), path)
	}
	if !strings.Contains(errOut, w.id.KeyID()) || !strings.Contains(errOut, witnessclient.KeysPath) {
		t.Errorf("the command did not tell the operator which key to compare where:\n%s", errOut)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	recorded := []string{audit.MethodCLI + " /cli/witness/enroll"}
	if diff := cmp.Diff(recorded, enrollEntries(t, db)); diff != "" {
		t.Errorf("the enrollment was not recorded once in the chain (-want +got):\n%s", diff)
	}

	tests := []struct {
		// URL is --witness.
		URL string
		// Pin is --witness-key.
		Pin string
		// Refresh is --refresh-keys.
		Refresh bool
		// WantErr is a fragment the refusal must contain, empty when it succeeds.
		WantErr string
		// WantOut is a fragment the printed JSON must contain on success.
		WantOut string
		// WantRequests is how many requests reach the witness.
		WantRequests int64
	}{{ // Test 0: Enrolling again answers the same.
		URL: w.url, WantOut: `"witness_key_id":"` + w.id.KeyID() + `"`, WantRequests: 1,
	}, { // Test 1: The pinned key is the witness's.
		URL: w.url, Pin: w.id.KeyID(), WantOut: `"key_id":"` + producer.KeyID() + `"`,
		WantRequests: 1,
	}, { // Test 2: A pin the witness does not sign with.
		URL: w.url, Pin: "sha256:" + strings.Repeat("0", 64), WantErr: "not the pinned",
		WantRequests: 1,
	}, { // Test 3: A pin out of form is refused before any request.
		URL: w.url, Pin: "abc", WantErr: "not a witness key id",
	}, { // Test 4: Another witness than the one enrolled with.
		URL: "https://other-witness.example.com", WantErr: "enrolled with the witness at",
	}, { // Test 5: No witness named.
		WantErr: "--witness is required",
	}, { // Test 6: Refreshing an unrotated key list changes nothing.
		Refresh: true, WantOut: `"changed":false`, WantRequests: 1,
	}, { // Test 7: Refreshing against another witness is refused.
		Refresh: true, URL: "https://other-witness.example.com", WantErr: "enrolled with the witness",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			before := w.requests.Load()
			out, errOut, err := runEnroll(t, db, test.URL, test.Pin, test.Refresh)
			if test.WantErr == "" {
				if err != nil || !strings.Contains(out, test.WantOut) {
					t.Errorf("runWitnessEnroll() = %v, printed %q; want success printing %q\n%s",
						err, out, test.WantOut, errOut)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("runWitnessEnroll() = %v, want a refusal naming %q", err, test.WantErr)
			}
			if got := w.requests.Load() - before; got != test.WantRequests {
				t.Errorf("the witness got %d requests, want %d", got, test.WantRequests)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if !bytes.Equal(written, after) {
				t.Errorf("the enrollment file changed:\n%s\nto\n%s", written, after)
			}
		})
	}

	// Another witness answering at the enrolled address signs with a key the held list does not
	// accept, so enrolling again there is refused and the held enrollment stays.
	stranger := newEnrollWitness(t)
	e.Witness = stranger.url
	if err := e.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	moved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if _, _, err := runEnroll(t, db, stranger.url, "", false); !errors.Is(err,
		witnessclient.ErrWitnessKey) {
		t.Errorf("runWitnessEnroll() answered by another witness's key = %v, want ErrWitnessKey", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(moved, after) {
		t.Errorf("a refused enrollment changed the held one: %v", err)
	}
	// Enrolling again with the same answer, refreshing an unchanged list, and every refusal
	// recorded nothing more.
	if diff := cmp.Diff(recorded, enrollEntries(t, db)); diff != "" {
		t.Errorf("chain entries after the repeats and refusals mismatch (-want +got):\n%s", diff)
	}
}

// TestWitnessEnrollRefusesARotatedIdentity pins design 3.1a at the command: an install whose key
// was changed under its kept install id through SWITCHTENDER_AUDIT_KEY is refused with the
// rotated-identity wording before any request, and no enrollment is written.
func TestWitnessEnrollRefusesARotatedIdentity(t *testing.T) {
	enrollTestEnv(t)
	w := newEnrollWitness(t)
	kid, priv := policyLicenseKey(t)
	lic := signedLicense(t, kid, priv, "lic_enroll_rotated", license.TierTeam,
		[]string{license.AddonHostedWitness}, time.Now().Add(365*24*time.Hour))
	db := enrollInstall(t, lic)
	dir := filepath.Dir(db)
	if _, err := identity.LoadFile(dir); err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	next, err := identity.LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	// A seed in the environment signs over the stored install id when the identity directory is
	// named, which is the rotation identity.Load documents.
	t.Setenv(identityDirEnv, dir)
	t.Setenv(identity.KeyEnv, next.Seed)
	_, _, err = runEnroll(t, db, w.url, "", false)
	if !errors.Is(err, witnessclient.ErrEnrollment) ||
		!strings.Contains(err.Error(), "rotated identity") {
		t.Errorf("runWitnessEnroll() = %v, want the rotated-identity refusal", err)
	}
	if got := w.requests.Load(); got != 0 {
		t.Errorf("the witness got %d requests, want none before the refusal", got)
	}
	if _, err := os.Stat(filepath.Join(dir, witnessclient.EnrollmentFile)); !errors.Is(err,
		fs.ErrNotExist) {
		t.Errorf("a refused enrollment left a file behind: %v", err)
	}
}

// TestWitnessEnrollRefusesPlainHTTPOffLoopback pins the transport rule on the command: a witness
// URL that is plain http to a host off the loopback is refused before any request, so the license
// file never crosses the network in the clear, and the install directory is left as it was. A held
// enrollment naming such a URL is refused by --refresh-keys the same way. Plain http to a loopback
// address, as every test witness here is, enrolls.
func TestWitnessEnrollRefusesPlainHTTPOffLoopback(t *testing.T) {
	enrollTestEnv(t)
	w := newEnrollWitness(t)
	kid, priv := policyLicenseKey(t)
	lic := signedLicense(t, kid, priv, "lic_enroll_http", license.TierEnterprise, nil,
		time.Now().Add(365*24*time.Hour))
	db := enrollInstall(t, lic)
	dir := filepath.Dir(db)
	before := dirDigest(t, dir)
	for _, raw := range []string{"http://10.0.0.5:9440", "http://witness.example.com"} {
		_, _, err := runEnroll(t, db, raw, "", false)
		if !errors.Is(err, witnessclient.ErrEnrollment) || !strings.Contains(err.Error(),
			"plain http") {
			t.Errorf("runWitnessEnroll(%s) = %v, want the transport refusal", raw, err)
		}
	}
	if diff := cmp.Diff(before, dirDigest(t, dir)); diff != "" {
		t.Errorf("a refused enrollment changed the install directory (-before +after):\n%s", diff)
	}
	if _, _, err := runEnroll(t, db, w.url, "", false); err != nil {
		t.Fatalf("runWitnessEnroll() over loopback http = %v, want an enrollment", err)
	}
	path := filepath.Join(dir, witnessclient.EnrollmentFile)
	enr, err := witnessclient.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	enr.Witness = "http://10.0.0.5:9440"
	if err := enr.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sent := w.requests.Load()
	if _, _, err := runEnroll(t, db, "", "", true); !errors.Is(err,
		witnessclient.ErrEnrollment) || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("runWitnessEnroll(--refresh-keys) over plain http = %v, want the transport "+
			"refusal", err)
	}
	if got := w.requests.Load(); got != sent {
		t.Errorf("the refused refresh sent %d requests, want none", got-sent)
	}
}
