package witness

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/identity"
)

// TestEnrollmentAnswersTheSameBytesAgain pins that enrollment is idempotent: the same install
// enrolling again, an hour later or after a restart, gets the byte-identical signed answer, which
// names the producer key it bound, the label, the push path, and the witness key list.
func TestEnrollmentAnswersTheSameBytesAgain(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 4)
	req := pr.enrollment(t, "lic_same", time.Time{})
	code, first := w.post(t, "/v1/enroll", req)
	if code != http.StatusOK {
		t.Fatalf("enroll = %d %s, want 200", code, first)
	}
	var resp EnrollResponse
	if err := json.Unmarshal(first, &resp); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	signer, err := VerifyEnrollResponse(&resp)
	if err != nil || signer != w.id.KeyID() {
		t.Fatalf("VerifyEnrollResponse() = %s, %v, want signed by this witness", signer, err)
	}
	want := EnrollResponse{InstallID: pr.label, KeyID: pr.keyID(),
		PublicKey: pr.id.PublicKeyBase64(), EnrolledAt: stamp(pushEpoch),
		MaxInstalls: DefaultMaxInstalls, PushPath: "/v1/installs/" + pr.keyID() + "/beats"}
	got := resp
	got.Switchtender, got.Witness, got.Sig = "", WitnessKeys{}, ""
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("answer (-want +got):\n%s", diff)
	}
	w.clock.Add(time.Hour)
	if code, again := w.post(t, "/v1/enroll", req); code != http.StatusOK || string(again) !=
		string(first) {
		t.Errorf("enrolling again = %d, answer changed:\n%s\n%s", code, first, again)
	}
	w.restart(t)
	if code, again := w.post(t, "/v1/enroll", req); code != http.StatusOK || string(again) !=
		string(first) {
		t.Errorf("enrolling after a restart = %d, answer changed:\n%s\n%s", code, first, again)
	}
}

// TestEnrollmentRefusesAnIdTheKeyWasNotBornTo pins that a key enrolls only under an id it was born
// to. A rotated identity, which kept its old id over a new key, and a lift of another install's id
// are both refused with nothing written, because from the bytes alone the two look the same.
func TestEnrollmentRefusesAnIdTheKeyWasNotBornTo(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	old, rotated := newProducer(t, 4), newProducer(t, 4)
	tests := []struct {
		// Label is the id the request names.
		Label string
	}{{ // Test 0: The rotated shape: the old key's wide id over the new key.
		Label: old.label,
	}, { // Test 1: The old key's narrow id over the new key.
		Label: identity.LegacyInstallIDFromKey(old.id.Public()),
	}, { // Test 2: An id no key was born to.
		Label: "in_" + strings.Repeat("0", 32),
	}}
	for testNum, test := range tests {
		before := snapshotDir(t, w.dir)
		req := rotated.enrollment(t, "lic_rotated", time.Time{})
		req.InstallID = test.Label
		if err := SignEnroll(&req, rotated.id); err != nil {
			t.Fatalf("SignEnroll() error = %v", err)
		}
		if code, raw := w.post(t, "/v1/enroll", req); code != http.StatusBadRequest {
			t.Errorf("test %d: status = %d %s, want 400", testNum, code, raw)
		}
		if _, err := w.svc.Enroll(req); !errors.Is(err, ErrNotBorn) {
			t.Errorf("test %d: Enroll() error = %v, want %v", testNum, err, ErrNotBorn)
		}
		if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
			t.Errorf("test %d: the refusal wrote (-before +after):\n%s", testNum, diff)
		}
	}
}

// TestEnrollmentStoresOnlyTheInstallIdentity is invariant 4: after an enrollment and a push over
// the API, the state directory holds none of what the request carried beyond the install id, the
// key, the license id, and its term. No organization, contact, URL, client address, request time,
// or license signature is written, and the enrollment record holds exactly its six members.
func TestEnrollmentStoresOnlyTheInstallIdentity(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 4)
	license := `{"claims":{"v":1,"id":"lic_identity","org":"Acme Witness Org","tier":"team",` +
		`"hosts":"250","contact":"ops@acme.example","url":"https://acme.example/st",` +
		`"addons":["hosted-witness"],"expires":"2099-01-01T00:00:00Z"},` +
		`"sig":"bGljZW5zZSBzaWduYXR1cmUgYnl0ZXM="}`
	req := EnrollRequest{InstallID: pr.label, License: json.RawMessage(license),
		At: "2026-10-20T21:13:14Z"}
	if err := SignEnroll(&req, pr.id); err != nil {
		t.Fatalf("SignEnroll() error = %v", err)
	}
	if code, raw := w.post(t, "/v1/enroll", req); code != http.StatusOK {
		t.Fatalf("enroll = %d %s", code, raw)
	}
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("push = %d %s", code, raw)
	}
	forbidden := []string{"Acme Witness Org", "ops@acme.example", "acme.example", "https://",
		"bGljZW5zZSBzaWduYXR1cmUgYnl0ZXM=", req.Sig, req.At, "127.0.0.1", "localhost",
		w.api.URL}
	for path, content := range snapshotDir(t, w.dir) {
		for _, s := range forbidden {
			if strings.Contains(content, s) {
				t.Errorf("%s holds %q, which the witness must not keep", path, s)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(w.recordDir(pr), enrollmentFile))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var members map[string]any
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	got := make([]string, 0, len(members))
	for k := range members {
		got = append(got, k)
	}
	slices.Sort(got)
	want := []string{"enrolled_at", "expires", "install_id", "key_id", "license_id", "public_key"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("enrollment record members (-want +got):\n%s", diff)
	}
}

// TestEnrollmentKeepsOneLabelPerKey pins that one key keeps one label and one memory. The same key
// enrolling under the other form it was born to is refused naming the label it holds, with nothing
// written, and the first enrollment still answers as before.
func TestEnrollmentKeepsOneLabelPerKey(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 4)
	w.enroll(t, pr, "lic_label")
	before := snapshotDir(t, w.dir)
	narrow := pr.enrollment(t, "lic_label", time.Time{})
	narrow.InstallID = identity.LegacyInstallIDFromKey(pr.id.Public())
	if err := SignEnroll(&narrow, pr.id); err != nil {
		t.Fatalf("SignEnroll() error = %v", err)
	}
	code, raw := w.post(t, "/v1/enroll", narrow)
	if code != http.StatusConflict || !strings.Contains(string(raw), pr.label) {
		t.Errorf("enroll under the narrow id = %d %s, want 409 naming %s", code, raw, pr.label)
	}
	if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
		t.Errorf("the refusal wrote (-before +after):\n%s", diff)
	}
	if _, err := w.svc.Enroll(pr.enrollment(t, "lic_label", time.Time{})); err != nil {
		t.Errorf("enrolling under the held label again: %v", err)
	}
}

// TestEnrollmentIsCappedPerLicense pins the cap: one license enrolls up to its cap of installs and
// is refused past it with nothing written, while a re-enrollment of an install already counted and
// an install under another license still enroll. The cap survives a restart.
func TestEnrollmentIsCappedPerLicense(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t, WithAcceptEnrollments(2))
	first, second, third := newProducer(t, 4), newProducer(t, 4), newProducer(t, 4)
	w.enroll(t, first, "lic_cap")
	w.enroll(t, second, "lic_cap")
	before := snapshotDir(t, w.dir)
	code, raw := w.post(t, "/v1/enroll", third.enrollment(t, "lic_cap", time.Time{}))
	if code != http.StatusForbidden {
		t.Errorf("third install on one license = %d %s, want 403", code, raw)
	}
	if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
		t.Errorf("the refusal wrote (-before +after):\n%s", diff)
	}
	if _, err := w.svc.Enroll(first.enrollment(t, "lic_cap", time.Time{})); err != nil {
		t.Errorf("re-enrolling a counted install: %v", err)
	}
	w.restart(t, WithAcceptEnrollments(2))
	if _, err := w.svc.Enroll(third.enrollment(t, "lic_cap", time.Time{})); !errors.Is(err, ErrCap) {
		t.Errorf("third install after a restart: %v, want %v", err, ErrCap)
	}
	if _, err := w.svc.Enroll(third.enrollment(t, "lic_other", time.Time{})); err != nil {
		t.Errorf("an install under another license: %v", err)
	}
}

// TestEnrollmentRefusalsWriteNothing pins every refusal an enrollment can get, each with its status
// and nothing written.
func TestEnrollmentRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	pr := newProducer(t, 4)
	signedBy := func(r EnrollRequest, id identity.Identity) EnrollRequest {
		if err := SignEnroll(&r, id); err != nil {
			t.Fatalf("SignEnroll() error = %v", err)
		}
		return r
	}
	tampered := pr.enrollment(t, "lic_refuse", time.Time{})
	tampered.At = "2026-10-21T12:00:01Z"
	wrongTag := pr.enrollment(t, "lic_refuse", time.Time{})
	wrongTag.Switchtender = PushVersion
	noLicense := signedBy(EnrollRequest{InstallID: pr.label, At: "2026-10-21T12:00:00Z"}, pr.id)
	tests := []struct {
		// Closed is whether the witness only watches and accepts no enrollments.
		Closed bool
		// Request is what is sent.
		Request EnrollRequest
		// WantCode is the answer.
		WantCode int
		// Want is the error Enroll returns.
		Want error
	}{{ // Test 0: A witness that accepts no enrollments.
		Closed: true, Request: pr.enrollment(t, "lic_refuse", time.Time{}),
		WantCode: http.StatusForbidden, Want: ErrClosed,
	}, { // Test 1: A request edited after signing.
		Request: tampered, WantCode: http.StatusUnauthorized, Want: ErrSignature,
	}, { // Test 2: A request under another tag.
		Request: wrongTag, WantCode: http.StatusBadRequest, Want: ErrMalformed,
	}, { // Test 3: A request the policy refuses.
		Request: noLicense, WantCode: http.StatusForbidden, Want: ErrEntitlement,
	}, { // Test 4: A license whose term has ended.
		Request:  pr.enrollment(t, "lic_refuse", pushEpoch.Add(-time.Hour)),
		WantCode: http.StatusPaymentRequired, Want: ErrLapsed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			w := newPushWitness(t)
			if test.Closed {
				w = newClosedWitness(t)
			}
			before := snapshotDir(t, w.dir)
			if code, raw := w.post(t, "/v1/enroll", test.Request); code != test.WantCode {
				t.Errorf("status = %d %s, want %d", code, raw, test.WantCode)
			}
			if _, err := w.svc.Enroll(test.Request); !errors.Is(err, test.Want) {
				t.Errorf("Enroll() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
				t.Errorf("the refusal wrote (-before +after):\n%s", diff)
			}
		})
	}
}

// newClosedWitness starts a witness that watches one feed and accepts no enrollments, with its API
// served.
func newClosedWitness(t *testing.T) *pushWitness {
	t.Helper()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	w := &pushWitness{id: testIdentity(t), dir: t.TempDir(), clock: &testClock{now: pushEpoch}}
	svc, err := NewService(w.id, w.dir, time.Minute, []string{feed.srv.URL}, nil, feed.srv.Client(),
		WithServiceClock(w.clock.Now), WithWitnessKeys([]WitnessKey{WitnessKeyOf(w.id, pushEpoch)}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	w.svc = svc
	w.api = newTestServer(svc.Handler())
	t.Cleanup(w.api.Close)
	return w
}

// TestEnrollmentRecordsANewerTerm pins that a renewal reaches the record: the same install
// enrolling with a later term updates the license id and expiry and keeps its enrolled_at, an older
// term changes nothing, and pushes past the old term's grace are then accepted.
func TestEnrollmentRecordsANewerTerm(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 4)
	oldTerm := pushEpoch.Add(24 * time.Hour)
	newTerm := pushEpoch.Add(400 * 24 * time.Hour)
	if _, err := w.svc.Enroll(pr.enrollment(t, "lic_2026", oldTerm)); err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	w.clock.Add(time.Hour)
	if _, err := w.svc.Enroll(pr.enrollment(t, "lic_2027", newTerm)); err != nil {
		t.Fatalf("Enroll() with the renewal error = %v", err)
	}
	if _, err := w.svc.Enroll(pr.enrollment(t, "lic_2026", oldTerm)); err != nil {
		t.Fatalf("Enroll() with the old license error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(w.recordDir(pr), enrollmentFile))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var rec Enrollment
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if rec.LicenseID != "lic_2027" || rec.Expires != stamp(newTerm) ||
		rec.EnrolledAt != stamp(pushEpoch) {
		t.Errorf("record = %+v, want the renewal's id and term and the first enrolled_at", rec)
	}
	w.clock.Set(oldTerm.Add(LapseGrace + 24*time.Hour))
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Errorf("push past the old term's grace = %d %s, want 200 under the renewal", code, raw)
	}
}

// TestTheKeyListIsPublishedSigned pins the well-known document: it is the witness's signed key
// list, it verifies, it names the current key among its keys, and any edit to it fails.
func TestTheKeyListIsPublishedSigned(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	code, raw := w.get(t, "/.well-known/loomseal.json")
	if code != http.StatusOK {
		t.Fatalf("GET key list = %d %s", code, raw)
	}
	var keys WitnessKeys
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode key list: %v", err)
	}
	if id, err := VerifyWitnessKeys(&keys); err != nil || id != w.id.KeyID() {
		t.Fatalf("VerifyWitnessKeys() = %s, %v, want this witness's key", id, err)
	}
	other := testIdentity(t)
	tests := []struct {
		// Mutate edits the list.
		Mutate func(k *WitnessKeys)
		// Want is the error verification returns.
		Want error
	}{{ // Test 0: A key added to the list.
		Mutate: func(k *WitnessKeys) { k.Keys = append(k.Keys, WitnessKeyOf(other, pushEpoch)) },
		Want:   ErrSignature,
	}, { // Test 1: The current key swapped for another, with its id.
		Mutate: func(k *WitnessKeys) { k.KeyID, k.PublicKey = other.KeyID(), other.PublicKeyBase64() },
		Want:   ErrMalformed,
	}, { // Test 2: A key id that is not the key's.
		Mutate: func(k *WitnessKeys) { k.KeyID = other.KeyID() }, Want: ErrMalformed,
	}, { // Test 3: Another tag.
		Mutate: func(k *WitnessKeys) { k.Switchtender = EnrolledVersion }, Want: ErrMalformed,
	}, { // Test 4: A valid-from date moved.
		Mutate: func(k *WitnessKeys) { k.Keys[0].ValidFrom = stamp(pushEpoch.Add(time.Hour)) },
		Want:   ErrSignature,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var edited WitnessKeys
			if err := json.Unmarshal(raw, &edited); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			test.Mutate(&edited)
			if _, err := VerifyWitnessKeys(&edited); !errors.Is(err, test.Want) {
				t.Errorf("VerifyWitnessKeys() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestAnEnrollmentAnswerBindsItsMembers pins that every member of the signed enrollment answer is
// covered: an edit to the bound key, the label, the push path, or the enrolled_at fails.
func TestAnEnrollmentAnswerBindsItsMembers(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr, other := newProducer(t, 4), newProducer(t, 4)
	resp, err := w.svc.Enroll(pr.enrollment(t, "lic_bind", time.Time{}))
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	tests := []struct {
		// Mutate edits the answer.
		Mutate func(r *EnrollResponse)
		// Want is the error verification returns.
		Want error
	}{{ // Test 0: The answer as signed.
		Mutate: func(*EnrollResponse) {}, Want: nil,
	}, { // Test 1: Another install's label.
		Mutate: func(r *EnrollResponse) { r.InstallID = other.label }, Want: ErrSignature,
	}, { // Test 2: Another key, with its id.
		Mutate: func(r *EnrollResponse) {
			r.KeyID, r.PublicKey = other.keyID(), other.id.PublicKeyBase64()
		},
		Want: ErrSignature,
	}, { // Test 3: A key id that is not the key's.
		Mutate: func(r *EnrollResponse) { r.KeyID = other.keyID() }, Want: ErrMalformed,
	}, { // Test 4: Another push path.
		Mutate: func(r *EnrollResponse) { r.PushPath = "/v1/installs/x/beats" }, Want: ErrSignature,
	}, { // Test 5: Another enrollment time.
		Mutate: func(r *EnrollResponse) { r.EnrolledAt = stamp(pushEpoch.Add(time.Hour)) },
		Want:   ErrSignature,
	}, { // Test 6: Another cap.
		Mutate: func(r *EnrollResponse) { r.MaxInstalls++ }, Want: ErrSignature,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			edited := *resp
			edited.Witness.Keys = slices.Clone(resp.Witness.Keys)
			test.Mutate(&edited)
			_, err := VerifyEnrollResponse(&edited)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyEnrollResponse() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestNewServiceRefusesAnEnrollmentItCannotServe pins the constructor's enrollment checks: a
// service that accepts enrollments needs a policy, a cap of at least one, and a published key list
// naming its own key, and a service that neither watches nor enrolls has nothing to do.
func TestNewServiceRefusesAnEnrollmentItCannotServe(t *testing.T) {
	t.Parallel()
	id, other := testIdentity(t), testIdentity(t)
	ownKeys := WithWitnessKeys([]WitnessKey{WitnessKeyOf(id, pushEpoch)})
	tests := []struct {
		// Servers are the watched servers.
		Servers []string
		// Options configure the service.
		Options []ServiceOption
		// WantErr is whether construction is refused.
		WantErr bool
	}{{ // Test 0: Everything an enrolling service needs.
		Options: []ServiceOption{WithAcceptEnrollments(1), WithEnrollmentPolicy(OpenEnrollment),
			ownKeys},
	}, { // Test 1: No policy.
		Options: []ServiceOption{WithAcceptEnrollments(1), ownKeys}, WantErr: true,
	}, { // Test 2: A cap of zero.
		Options: []ServiceOption{WithAcceptEnrollments(0), WithEnrollmentPolicy(OpenEnrollment),
			ownKeys},
		WantErr: true,
	}, { // Test 3: No key list.
		Options: []ServiceOption{WithAcceptEnrollments(1), WithEnrollmentPolicy(OpenEnrollment)},
		WantErr: true,
	}, { // Test 4: A key list without this witness's key.
		Options: []ServiceOption{WithAcceptEnrollments(1), WithEnrollmentPolicy(OpenEnrollment),
			WithWitnessKeys([]WitnessKey{WitnessKeyOf(other, pushEpoch)})},
		WantErr: true,
	}, { // Test 5: Neither watching nor enrolling.
		Options: []ServiceOption{ownKeys}, WantErr: true,
	}, { // Test 6: Watching without enrolling needs nothing more.
		Servers: []string{"https://st.example"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s, err := NewService(id, t.TempDir(), time.Minute, test.Servers, nil, nil, test.Options...)
			if (err != nil) != test.WantErr {
				t.Errorf("NewService() error = %v, want error %v", err, test.WantErr)
			}
			if s != nil {
				s.Close()
			}
		})
	}
}

// TestEnrollmentsAreRateLimitedPerLicense pins the enrollment limiter: one license spends its burst
// and is refused with a 429 and nothing written, while another license still enrolls.
func TestEnrollmentsAreRateLimitedPerLicense(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 4)
	req := pr.enrollment(t, "lic_busy", time.Time{})
	for i := 0; i < enrollBurst; i++ {
		if _, err := w.svc.Enroll(req); err != nil {
			t.Fatalf("enrollment %d of the burst: %v", i+1, err)
		}
	}
	before := snapshotDir(t, w.dir)
	if code, raw := w.post(t, "/v1/enroll", newProducer(t, 4).enrollment(t, "lic_busy",
		time.Time{})); code != http.StatusTooManyRequests {
		t.Errorf("enrollment past the burst = %d %s, want 429", code, raw)
	}
	if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
		t.Errorf("the refusal wrote (-before +after):\n%s", diff)
	}
	if _, err := w.svc.Enroll(newProducer(t, 4).enrollment(t, "lic_calm", time.Time{})); err != nil {
		t.Errorf("an enrollment under another license: %v", err)
	}
}

// TestAStartRefusesARecordThatFailsItsChecks pins that a record that does not hold together stops
// the start rather than being skipped, while a directory with no enrollment in it, which a crash
// mid-enrollment leaves, is passed over.
func TestAStartRefusesARecordThatFailsItsChecks(t *testing.T) {
	t.Parallel()
	pr, other := newProducer(t, 4), newProducer(t, 4)
	tests := []struct {
		// Edit changes the record on disk.
		Edit func(t *testing.T, rec *Enrollment, dir string)
		// WantErr is whether the start is refused.
		WantErr bool
	}{{ // Test 0: The record as written.
		Edit: func(*testing.T, *Enrollment, string) {},
	}, { // Test 1: Another key filed under this key's name.
		Edit: func(_ *testing.T, rec *Enrollment, _ string) {
			rec.KeyID, rec.PublicKey = other.keyID(), other.id.PublicKeyBase64()
		},
		WantErr: true,
	}, { // Test 2: A label the key was not born to.
		Edit:    func(_ *testing.T, rec *Enrollment, _ string) { rec.InstallID = other.label },
		WantErr: true,
	}, { // Test 3: A checkpoint signed by another witness.
		Edit: func(t *testing.T, _ *Enrollment, dir string) {
			c := &Checkpoint{Server: pr.label, InstallID: pr.label, Recent: map[int64]Observed{}}
			if err := Save(filepath.Join(dir, installCheckpointFile), c, testIdentity(t)); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		},
		WantErr: true,
	}, { // Test 4: No enrollment at all.
		Edit: func(t *testing.T, _ *Enrollment, dir string) {
			if err := os.Remove(filepath.Join(dir, enrollmentFile)); err != nil {
				t.Fatalf("Remove() error = %v", err)
			}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			w := newPushWitness(t)
			w.enroll(t, pr, "lic_start")
			w.api.Close()
			w.svc.Close()
			dir := w.recordDir(pr)
			raw, err := os.ReadFile(filepath.Join(dir, enrollmentFile))
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			var rec Enrollment
			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			test.Edit(t, &rec, dir)
			if _, err := os.Stat(filepath.Join(dir, enrollmentFile)); err == nil {
				if err := writeEnrollment(dir, rec); err != nil {
					t.Fatalf("writeEnrollment() error = %v", err)
				}
			}
			s, err := NewService(w.id, w.dir, time.Minute, nil, nil, nil,
				WithAcceptEnrollments(1), WithEnrollmentPolicy(testPolicy),
				WithWitnessKeys([]WitnessKey{WitnessKeyOf(w.id, pushEpoch)}))
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			defer s.Close()
			if err := s.Start(); (err != nil) != test.WantErr {
				t.Errorf("Start() error = %v, want error %v", err, test.WantErr)
			}
		})
	}
}
