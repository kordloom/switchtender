package witnessclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/identity"
)

// TestEnrollWritesWhatTheWitnessBound pins an enrollment against a push witness: the answer binds
// this install's producer key and label under the witness's key, the file round-trips, enrolling
// again answers the same, and the enrollment follows the one already held.
func TestEnrollWritesWhatTheWitnessBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newTestWitness(t)
	in := newTestInstall(t)
	e := in.enroll(t, w)
	want := Enrollment{Witness: w.url, Keys: e.Keys, InstallID: in.id.InstallID,
		KeyID: in.id.KeyID(), PublicKey: in.id.PublicKeyBase64(), EnrolledAt: e.EnrolledAt,
		PushPath: "/v1/installs/" + in.id.KeyID() + "/beats"}
	if diff := cmp.Diff(want, *e); diff != "" {
		t.Errorf("Enroll() mismatch (-want +got):\n%s", diff)
	}
	if e.Keys.KeyID != w.id.KeyID() || e.EnrolledAt == "" {
		t.Errorf("Enroll() witness key %s at %q, want %s and a time", e.Keys.KeyID, e.EnrolledAt,
			w.id.KeyID())
	}
	loaded, err := Load(in.path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if diff := cmp.Diff(e, loaded); diff != "" {
		t.Errorf("Load() after Save() mismatch (-want +got):\n%s", diff)
	}
	if err := e.Pin(w.id.KeyID()); err != nil {
		t.Errorf("Pin() of the witness's own key = %v", err)
	}
	if err := e.Pin(in.id.KeyID()); !errors.Is(err, ErrWitnessKey) {
		t.Errorf("Pin() of another key = %v, want ErrWitnessKey", err)
	}
	again, err := Enroll(ctx, testHTTPClient(), w.url+"/", in.id, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Enroll() again error = %v", err)
	}
	if diff := cmp.Diff(e, again); diff != "" {
		t.Errorf("Enroll() again mismatch (-want +got):\n%s", diff)
	}
	if err := again.Follows(loaded); err != nil {
		t.Errorf("Follows() of the same enrollment = %v", err)
	}
}

// TestEnrollRefusesARotatedIdentityBeforeAnyRequest pins design 3.1a's install-side rule: an
// identity whose install id was not born to its key is refused with the rotated-identity wording,
// and nothing reaches the witness.
func TestEnrollRefusesARotatedIdentityBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	w := newTestWitness(t)
	born, err := identity.LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	other, err := identity.LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	// The key the install signs with now, kept under the id another key was born to.
	dir := t.TempDir()
	stored, err := json.Marshal(map[string]string{"install_id": other.InstallID, "seed": born.Seed})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, identity.File), stored, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	rotated, err := identity.LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if rotated.InstallID != other.InstallID || rotated.KeyID() != born.KeyID() {
		t.Fatalf("the rotated identity is %s under %s, want %s under %s", rotated.InstallID,
			rotated.KeyID(), other.InstallID, born.KeyID())
	}
	_, err = Enroll(context.Background(), testHTTPClient(), w.url, rotated, nil, time.Now())
	if !errors.Is(err, ErrEnrollment) || !strings.Contains(err.Error(), "rotated identity") {
		t.Errorf("Enroll() of a rotated identity = %v, want the rotated-identity refusal", err)
	}
	if got := w.requests.Load(); got != 0 {
		t.Errorf("the witness got %d requests, want none before the refusal", got)
	}
}

// TestEnrollmentFollowsOnlyAnAcceptedKeyList pins that a second enrollment never replaces the held
// one with another witness, another identity, or a key list no held key signed.
func TestEnrollmentFollowsOnlyAnAcceptedKeyList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first, second := newTestWitness(t), newTestWitness(t)
	in := newTestInstall(t)
	held := in.enroll(t, first)
	fromSecond, err := Enroll(ctx, testHTTPClient(), second.url, in.id, nil, time.Now())
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	sameURL := *fromSecond
	sameURL.Witness = held.Witness
	otherInstall := newTestInstall(t)
	otherHeld, err := Enroll(ctx, testHTTPClient(), first.url, otherInstall.id, nil, time.Now())
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	tests := []struct {
		// Next is the enrollment offered in place of the held one.
		Next *Enrollment
		// Want is the error class, nil when it follows.
		Want error
	}{{ // Test 0: The same answer again.
		Next: held, Want: nil,
	}, { // Test 1: Another witness's address.
		Next: fromSecond, Want: ErrEnrollment,
	}, { // Test 2: Another witness's key list at the held address.
		Next: &sameURL, Want: ErrWitnessKey,
	}, { // Test 3: Another install's enrollment with the same witness.
		Next: otherHeld, Want: ErrEnrollment,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Next.Follows(held)
			if (test.Want == nil) != (err == nil) || (test.Want != nil && !errors.Is(err, test.Want)) {
				t.Errorf("Follows() = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestEnrollRefreshKeysKeepsOnlyAnEndorsedList pins --refresh-keys against a list no held key
// signed: the list another witness publishes at the enrolled address is refused, and the file is
// left byte for byte as it was. A pin the published list does not carry is refused the same way.
func TestEnrollRefreshKeysKeepsOnlyAnEndorsedList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first, stranger := newTestWitness(t), newTestWitness(t)
	in := newTestInstall(t)
	e := in.enroll(t, first)
	if _, changed, err := RefreshKeys(ctx, testHTTPClient(), in.path, first.id.KeyID()); err != nil ||
		changed {
		t.Fatalf("RefreshKeys() of the same list = %v, %v; want unchanged", changed, err)
	}
	if _, _, err := RefreshKeys(ctx, testHTTPClient(), in.path, stranger.id.KeyID()); !errors.Is(err,
		ErrWitnessKey) {
		t.Errorf("RefreshKeys() with a pin the list does not carry = %v, want ErrWitnessKey", err)
	}
	e.Witness = stranger.url
	if err := e.Save(in.path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	before, err := os.ReadFile(in.path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if _, _, err := RefreshKeys(ctx, testHTTPClient(), in.path, ""); !errors.Is(err, ErrWitnessKey) {
		t.Errorf("RefreshKeys() of a stranger's list = %v, want ErrWitnessKey", err)
	}
	after, err := os.ReadFile(in.path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a refused refresh changed the enrollment file:\n%s\nto\n%s", before, after)
	}
}

// TestRequireSecureURL pins the transport rule for a witness URL: https anywhere, plain http only
// to a loopback address, which never leaves the machine.
func TestRequireSecureURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the normalized witness URL.
		In string
		// Want is the error the rule returns, nil for a URL it accepts.
		Want error
	}{{ // Test 0: https to a public host.
		In: "https://witness.example.com",
	}, { // Test 1: https to a private address.
		In: "https://10.0.0.5:9440",
	}, { // Test 2: Plain http to 127.0.0.1.
		In: "http://127.0.0.1:9440",
	}, { // Test 3: Plain http to another address in 127.0.0.0/8.
		In: "http://127.8.9.10:9440",
	}, { // Test 4: Plain http to ::1.
		In: "http://[::1]:9440",
	}, { // Test 5: Plain http to localhost, in any case.
		In: "http://LocalHost:9440",
	}, { // Test 6: Plain http to a private network address.
		In: "http://10.0.0.5:9440", Want: ErrEnrollment,
	}, { // Test 7: Plain http to a public host.
		In: "http://witness.example.com", Want: ErrEnrollment,
	}, { // Test 8: Plain http to a name that only starts like localhost.
		In: "http://localhost.example.com", Want: ErrEnrollment,
	}, { // Test 9: Plain http to an address just outside 127.0.0.0/8.
		In: "http://128.0.0.1", Want: ErrEnrollment,
	}, { // Test 10: Plain http to the unspecified address.
		In: "http://0.0.0.0:9440", Want: ErrEnrollment,
	}, { // Test 11: Plain http to the IPv4-mapped loopback.
		In: "http://[::ffff:127.0.0.1]:9440",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := RequireSecureURL(test.In)
			if !errors.Is(err, test.Want) || (err == nil) != (test.Want == nil) {
				t.Errorf("RequireSecureURL(%q) = %v, want %v", test.In, err, test.Want)
			}
		})
	}
}

// TestEnrollRefusesPlainHTTPOffLoopback pins that Enroll sends nothing to a plain-http witness that
// is not on a loopback address, since the request carries the license file.
func TestEnrollRefusesPlainHTTPOffLoopback(t *testing.T) {
	t.Parallel()
	in := newTestInstall(t)
	_, err := Enroll(context.Background(), testHTTPClient(), "http://10.0.0.5:9440", in.id, nil,
		time.Now())
	if !errors.Is(err, ErrEnrollment) || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("Enroll() over plain http off loopback = %v, want the transport refusal", err)
	}
}

// TestEnrollNormalizesTheWitnessURL pins the witness URL rule.
func TestEnrollNormalizesTheWitnessURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the URL given.
		In string
		// WantURL is the base the requests go to, empty when refused.
		WantURL string
	}{{ // Test 0: A trailing slash is dropped.
		In: "https://witness.example.com/", WantURL: "https://witness.example.com",
	}, { // Test 1: A path prefix is kept.
		In: "https://example.com/witness/", WantURL: "https://example.com/witness",
	}, { // Test 2: Plain http on a private network.
		In: "http://10.0.0.5:9440", WantURL: "http://10.0.0.5:9440",
	}, { // Test 3: Another scheme.
		In: "ftp://witness.example.com",
	}, { // Test 4: No host.
		In: "https:///v1",
	}, { // Test 5: A query.
		In: "https://witness.example.com/?x=1",
	}, { // Test 6: A user.
		In: "https://u:p@witness.example.com",
	}, { // Test 7: Not a URL.
		In: "witness.example.com",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeURL(test.In)
			if got != test.WantURL || (err == nil) != (test.WantURL != "") {
				t.Errorf("NormalizeURL(%q) = %q, %v; want %q", test.In, got, err, test.WantURL)
			}
		})
	}
}
