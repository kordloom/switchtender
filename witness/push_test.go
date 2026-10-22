package witness

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/identity"
)

// testHash returns a 64 hex digest made of one repeated hex digit, distinct per digit.
func testHash(digit byte) string {
	return strings.Repeat(string(digit), 64)
}

// samplePush returns an unsigned push with every member set to a distinct value.
func samplePush() Push {
	return Push{
		InstallID:  "in_0123456789abcdef0123456789abcdef",
		BeatNumber: 412,
		At:         "2026-10-20T21:00:00Z",
		Seq:        90213,
		Head:       testHash('a'),
		Size:       90213,
		Root:       testHash('b'),
		FromSize:   90150,
		Proof:      []string{testHash('c'), testHash('d')},
	}
}

// jsonMembers returns the JSON member names a struct type serializes, sorted.
func jsonMembers(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		names = append(names, strings.Split(tag, ",")[0])
	}
	sort.Strings(names)
	return names
}

// TestPushWireShape pins the push body's member names to the ones design note section 3.4 gives.
func TestPushWireShape(t *testing.T) {
	t.Parallel()
	p := samplePush()
	if err := SignPush(&p, testIdentity(t)); err != nil {
		t.Fatalf("SignPush() error = %v", err)
	}
	raw, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"at", "beat", "from_size", "head", "install_id", "proof", "root", "seq",
		"sig", "size", "switchtender"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("push members mismatch (-want +got):\n%s", diff)
	}
	if got["switchtender"] != PushVersion {
		t.Errorf("switchtender = %v, want %s", got["switchtender"], PushVersion)
	}
}

// TestPushSignatureBindsEveryField signs one push and then changes each member in turn: the honest
// push verifies and every edit is refused. The table must name every member Push serializes, so a
// member added later without a row fails here.
func TestPushSignatureBindsEveryField(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	signed := samplePush()
	if err := SignPush(&signed, id); err != nil {
		t.Fatalf("SignPush() error = %v", err)
	}
	other := samplePush()
	other.Seq++
	if err := SignPush(&other, id); err != nil {
		t.Fatalf("SignPush() error = %v", err)
	}
	tests := []struct {
		Field  string
		Mutate func(p *Push)
		Want   error
	}{{ // Test 0: The honest push verifies.
		Field: "", Mutate: func(*Push) {}, Want: nil,
	}, { // Test 1: Another domain tag.
		Field: "switchtender", Mutate: func(p *Push) { p.Switchtender = EnrollVersion },
		Want: ErrMalformed,
	}, { // Test 2: Another install.
		Field: "install_id", Mutate: func(p *Push) { p.InstallID += "0" }, Want: ErrSignature,
	}, { // Test 3: Another beat number.
		Field: "beat", Mutate: func(p *Push) { p.BeatNumber++ }, Want: ErrSignature,
	}, { // Test 4: Another beat time.
		Field: "at", Mutate: func(p *Push) { p.At = "2026-10-20T21:00:01Z" }, Want: ErrSignature,
	}, { // Test 5: Another chain position.
		Field: "seq", Mutate: func(p *Push) { p.Seq-- }, Want: ErrSignature,
	}, { // Test 6: Another head link.
		Field: "head", Mutate: func(p *Push) { p.Head = testHash('e') }, Want: ErrSignature,
	}, { // Test 7: Another tree size.
		Field: "size", Mutate: func(p *Push) { p.Size++ }, Want: ErrSignature,
	}, { // Test 8: Another tree root.
		Field: "root", Mutate: func(p *Push) { p.Root = testHash('e') }, Want: ErrSignature,
	}, { // Test 9: Another proof base.
		Field: "from_size", Mutate: func(p *Push) { p.FromSize = 1 }, Want: ErrSignature,
	}, { // Test 10: One proof hash edited.
		Field: "proof", Mutate: func(p *Push) { p.Proof[1] = testHash('e') }, Want: ErrSignature,
	}, { // Test 11: One proof hash appended.
		Field: "proof", Mutate: func(p *Push) { p.Proof = append(p.Proof, testHash('e')) },
		Want: ErrSignature,
	}, { // Test 12: The proof dropped.
		Field: "proof", Mutate: func(p *Push) { p.Proof = nil }, Want: ErrSignature,
	}, { // Test 13: The proof reordered.
		Field: "proof", Mutate: func(p *Push) { p.Proof[0], p.Proof[1] = p.Proof[1], p.Proof[0] },
		Want: ErrSignature,
	}, { // Test 14: The signature replaced by another of the right length.
		Field: "sig", Mutate: func(p *Push) { p.Sig = other.Sig }, Want: ErrSignature,
	}, { // Test 15: The signature removed.
		Field: "sig", Mutate: func(p *Push) { p.Sig = "" }, Want: ErrSignature,
	}, { // Test 16: The signature carrying a line break.
		Field: "sig", Mutate: func(p *Push) { p.Sig = p.Sig[:8] + "\n" + p.Sig[8:] },
		Want: ErrMalformed,
	}}
	covered := map[string]bool{}
	for _, test := range tests {
		covered[test.Field] = true
	}
	for _, member := range jsonMembers(reflect.TypeFor[Push]()) {
		if !covered[member] {
			t.Errorf("no row edits push member %q, so nothing shows the signature binds it", member)
		}
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := signed
			p.Proof = append([]string(nil), signed.Proof...)
			test.Mutate(&p)
			err := VerifyPush(&p, id.Public())
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyPush() after editing %q error = %v, want %v", test.Field, err,
					test.Want)
			}
		})
	}
}

// TestPushNeedsTheEnrolledKey shows a push verifies only under the key that signed it, and that a
// key of the wrong size is refused rather than compared.
func TestPushNeedsTheEnrolledKey(t *testing.T) {
	t.Parallel()
	signer, other := testIdentity(t), testIdentity(t)
	p := samplePush()
	if err := SignPush(&p, signer); err != nil {
		t.Fatalf("SignPush() error = %v", err)
	}
	tests := []struct {
		Key  ed25519.PublicKey
		Want error
	}{{ // Test 0: The signing key.
		Key: signer.Public(), Want: nil,
	}, { // Test 1: Another install's key.
		Key: other.Public(), Want: ErrSignature,
	}, { // Test 2: No key.
		Key: nil, Want: ErrMalformed,
	}, { // Test 3: A key one byte short.
		Key: signer.Public()[:ed25519.PublicKeySize-1], Want: ErrMalformed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := VerifyPush(&p, test.Key)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyPush() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestPushRefusesAnotherDomainTag shows a signature made over the push's own members under any
// other domain tag does not verify as a push, even when the push then claims the push tag.
func TestPushRefusesAnotherDomainTag(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	tests := []struct {
		Tag string
	}{{ // Test 0: The enrollment tag.
		Tag: EnrollVersion,
	}, { // Test 1: The enrollment answer tag.
		Tag: EnrolledVersion,
	}, { // Test 2: The key list tag.
		Tag: KeysVersion,
	}, { // Test 3: A later push version.
		Tag: "witness-push/2",
	}, { // Test 4: The tag written as one string, as a reader might expect it.
		Tag: "switchtender witness-push/1",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := samplePush()
			sig, err := signTagged(test.Tag, &p, id.Private())
			if err != nil {
				t.Fatalf("signTagged() error = %v", err)
			}
			p.Switchtender, p.Sig = PushVersion, sig
			if err := VerifyPush(&p, id.Public()); !errors.Is(err, ErrSignature) {
				t.Errorf("VerifyPush() of a push signed under %q error = %v, want %v", test.Tag,
					err, ErrSignature)
			}
		})
	}
}

// TestPushBeatIsTheFeedShape shows a push presents its beat in the feed's own shape, so the
// existing checker reads it unchanged.
func TestPushBeatIsTheFeedShape(t *testing.T) {
	t.Parallel()
	p := samplePush()
	want := Beat{Beat: 412, At: "2026-10-20T21:00:00Z", Seq: 90213, Head: testHash('a')}
	if diff := cmp.Diff(want, p.Beat()); diff != "" {
		t.Errorf("Beat() mismatch (-want +got):\n%s", diff)
	}
}

// TestNilProtocolObjectsAreRefused shows every sign and verify function refuses a nil object rather
// than panicking.
func TestNilProtocolObjectsAreRefused(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	tests := []struct {
		Call func() error
	}{{ // Test 0: Signing no push.
		Call: func() error { return SignPush(nil, id) },
	}, { // Test 1: Verifying no push.
		Call: func() error { return VerifyPush(nil, id.Public()) },
	}, { // Test 2: Signing no enrollment.
		Call: func() error { return SignEnroll(nil, id) },
	}, { // Test 3: Verifying no enrollment.
		Call: func() error { return VerifyEnroll(nil) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := test.Call(); !errors.Is(err, ErrMalformed) {
				t.Errorf("error = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

// sampleEnroll returns an unsigned enrollment request for installID carrying a license-shaped
// document.
func sampleEnroll(installID string) EnrollRequest {
	return EnrollRequest{
		InstallID: installID,
		License: json.RawMessage(`{"claims":{"v":1,"tier":"team",` +
			`"addons":["hosted-witness"]},"sig":"AA=="}`),
		At: "2026-10-20T21:00:00Z",
	}
}

// TestEnrollSignatureBindsEveryField signs one enrollment and then changes each member in turn: the
// honest request verifies and every edit is refused. The table must name every member the request
// serializes.
func TestEnrollSignatureBindsEveryField(t *testing.T) {
	t.Parallel()
	id, other := testIdentity(t), testIdentity(t)
	signed := sampleEnroll(id.InstallID)
	if err := SignEnroll(&signed, id); err != nil {
		t.Fatalf("SignEnroll() error = %v", err)
	}
	if signed.PublicKey != id.PublicKeyBase64() {
		t.Fatalf("SignEnroll() public_key = %q, want the signer's %q", signed.PublicKey,
			id.PublicKeyBase64())
	}
	tests := []struct {
		Field  string
		Mutate func(r *EnrollRequest)
		Want   error
	}{{ // Test 0: The honest request verifies.
		Field: "", Mutate: func(*EnrollRequest) {}, Want: nil,
	}, { // Test 1: Another domain tag.
		Field: "switchtender", Mutate: func(r *EnrollRequest) { r.Switchtender = PushVersion },
		Want: ErrMalformed,
	}, { // Test 2: Another install.
		Field: "install_id", Mutate: func(r *EnrollRequest) { r.InstallID += "0" },
		Want: ErrSignature,
	}, { // Test 3: No install.
		Field: "install_id", Mutate: func(r *EnrollRequest) { r.InstallID = "" },
		Want: ErrMalformed,
	}, { // Test 4: Another valid key swapped in.
		Field:  "public_key",
		Mutate: func(r *EnrollRequest) { r.PublicKey = other.PublicKeyBase64() },
		Want:   ErrSignature,
	}, { // Test 5: A key that is not one.
		Field: "public_key", Mutate: func(r *EnrollRequest) { r.PublicKey = "AAAA" },
		Want: ErrMalformed,
	}, { // Test 6: The license tier raised.
		Field: "license", Mutate: func(r *EnrollRequest) {
			r.License = json.RawMessage(strings.Replace(string(r.License), "team", "enterprise", 1))
		}, Want: ErrSignature,
	}, { // Test 7: The license dropped.
		Field: "license", Mutate: func(r *EnrollRequest) { r.License = nil }, Want: ErrSignature,
	}, { // Test 8: Another request time.
		Field: "at", Mutate: func(r *EnrollRequest) { r.At = "2026-10-20T21:00:01Z" },
		Want: ErrSignature,
	}, { // Test 9: The signature removed.
		Field: "sig", Mutate: func(r *EnrollRequest) { r.Sig = "" }, Want: ErrSignature,
	}, { // Test 10: The signature cut short.
		Field: "sig", Mutate: func(r *EnrollRequest) { r.Sig = r.Sig[:16] }, Want: ErrMalformed,
	}}
	covered := map[string]bool{}
	for _, test := range tests {
		covered[test.Field] = true
	}
	for _, member := range jsonMembers(reflect.TypeFor[EnrollRequest]()) {
		if !covered[member] {
			t.Errorf("no row edits enrollment member %q, so nothing shows the signature binds it",
				member)
		}
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := signed
			test.Mutate(&r)
			err := VerifyEnroll(&r)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyEnroll() after editing %q error = %v, want %v", test.Field, err,
					test.Want)
			}
		})
	}
}

// TestEnrollRefusesAnotherDomainTag shows a request whose members were signed under any other tag,
// the push tag among them, does not verify as an enrollment.
func TestEnrollRefusesAnotherDomainTag(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	tests := []struct {
		Tag string
	}{{ // Test 0: The push tag.
		Tag: PushVersion,
	}, { // Test 1: The enrollment answer tag.
		Tag: EnrolledVersion,
	}, { // Test 2: The tag written as one string.
		Tag: "switchtender witness-enroll/1",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := sampleEnroll(id.InstallID)
			r.PublicKey = id.PublicKeyBase64()
			sig, err := signTagged(test.Tag, &r, id.Private())
			if err != nil {
				t.Fatalf("signTagged() error = %v", err)
			}
			r.Switchtender, r.Sig = EnrollVersion, sig
			if err := VerifyEnroll(&r); !errors.Is(err, ErrSignature) {
				t.Errorf("VerifyEnroll() of a request signed under %q error = %v, want %v",
					test.Tag, err, ErrSignature)
			}
		})
	}
}

// TestEnrollRequiresTheIdTheKeyWasBornTo shows an honestly signed enrollment verifies only when its
// install id is one its key was born to, wide or narrow. Another key's id and a rotated identity's
// kept id are refused, so nobody can enroll under a name their key does not own.
func TestEnrollRequiresTheIdTheKeyWasBornTo(t *testing.T) {
	t.Parallel()
	signer, other := testIdentity(t), testIdentity(t)
	wide := identity.InstallIDFromKey(signer.Public())
	narrow := identity.LegacyInstallIDFromKey(signer.Public())
	rotatedWide, rotatedNarrow := other, other
	rotatedWide.InstallID, rotatedNarrow.InstallID = wide, narrow
	tests := []struct {
		InstallID string
		Signer    identity.Identity
		WantBorn  bool
		Want      error
	}{{ // Test 0: The wide id the signing key was born to.
		InstallID: wide, Signer: signer, WantBorn: true, Want: nil,
	}, { // Test 1: The legacy narrow id the signing key was born to.
		InstallID: narrow, Signer: signer, WantBorn: true, Want: nil,
	}, { // Test 2: Another key's wide id.
		InstallID: identity.InstallIDFromKey(other.Public()), Signer: signer, WantBorn: false,
		Want: ErrNotBorn,
	}, { // Test 3: Another key's narrow id.
		InstallID: identity.LegacyInstallIDFromKey(other.Public()), Signer: signer, WantBorn: false,
		Want: ErrNotBorn,
	}, { // Test 4: A rotated identity keeping its wide id over a new key.
		InstallID: rotatedWide.InstallID, Signer: rotatedWide, WantBorn: false, Want: ErrNotBorn,
	}, { // Test 5: A rotated identity keeping its narrow id over a new key.
		InstallID: rotatedNarrow.InstallID, Signer: rotatedNarrow, WantBorn: false, Want: ErrNotBorn,
	}, { // Test 6: The wide id written in upper case.
		InstallID: "in_" + strings.ToUpper(strings.TrimPrefix(wide, "in_")), Signer: signer,
		WantBorn: false, Want: ErrNotBorn,
	}, { // Test 7: No install id.
		InstallID: "", Signer: signer, WantBorn: false, Want: ErrMalformed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := BornTo(test.InstallID, test.Signer.Public()); got != test.WantBorn {
				t.Errorf("BornTo(%q) = %v, want %v", test.InstallID, got, test.WantBorn)
			}
			r := sampleEnroll(test.InstallID)
			if err := SignEnroll(&r, test.Signer); err != nil {
				t.Fatalf("SignEnroll() error = %v", err)
			}
			err := VerifyEnroll(&r)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyEnroll() of %q error = %v, want %v", test.InstallID, err, test.Want)
			}
		})
	}
}

// TestBornToNeedsAKeyAndAnID shows a missing id or a key that is not one is never born to anything,
// even though each derivation answers empty for such a key.
func TestBornToNeedsAKeyAndAnID(t *testing.T) {
	t.Parallel()
	pub := testIdentity(t).Public()
	tests := []struct {
		InstallID string
		Key       ed25519.PublicKey
	}{{ // Test 0: No id and no key.
		InstallID: "", Key: nil,
	}, { // Test 1: No id and a key one byte short.
		InstallID: "", Key: pub[:ed25519.PublicKeySize-1],
	}, { // Test 2: The whole key's id over the key one byte short.
		InstallID: identity.InstallIDFromKey(pub), Key: pub[:ed25519.PublicKeySize-1],
	}, { // Test 3: No id and a whole key.
		InstallID: "", Key: pub,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if BornTo(test.InstallID, test.Key) {
				t.Errorf("BornTo(%q, %d byte key) = true, want false", test.InstallID, len(test.Key))
			}
		})
	}
}

// TestEnrollResponseWireShape pins the enrollment answer's member names, the producer key echoed
// beside the label among them.
func TestEnrollResponseWireShape(t *testing.T) {
	t.Parallel()
	want := []string{"enrolled_at", "install_id", "key_id", "max_installs", "public_key",
		"push_path", "sig", "switchtender", "witness"}
	if diff := cmp.Diff(want, jsonMembers(reflect.TypeFor[EnrollResponse]())); diff != "" {
		t.Errorf("enrollment answer members mismatch (-want +got):\n%s", diff)
	}
}

// TestTaggedContentCoversEveryMemberButSig pins the canonical bytes a protocol signature covers:
// sorted members, the domain member set to the tag whatever the object carried, and no sig.
func TestTaggedContentCoversEveryMemberButSig(t *testing.T) {
	t.Parallel()
	p := Push{Switchtender: "anything", InstallID: "in_x", BeatNumber: 1, At: "a", Seq: 2,
		Head: "h", Size: 3, Root: "r", FromSize: 1, Proof: []string{"p"}, Sig: "s"}
	got, err := taggedContent(PushVersion, &p)
	if err != nil {
		t.Fatalf("taggedContent() error = %v", err)
	}
	want := `{"at":"a","beat":1,"from_size":1,"head":"h","install_id":"in_x","proof":["p"],` +
		`"root":"r","seq":2,"size":3,"switchtender":"witness-push/1"}`
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Errorf("taggedContent() mismatch (-want +got):\n%s", diff)
	}
}

// TestTaggedContentRefusesWhatJCSCannotCarry shows a value the canonical form cannot represent is
// refused before anything is signed.
func TestTaggedContentRefusesWhatJCSCannotCarry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Value any
	}{{ // Test 0: Not an object.
		Value: []string{"a"},
	}, { // Test 1: A number past 2^53.
		Value: map[string]int64{"n": 1 << 60},
	}, { // Test 2: A value JSON cannot encode.
		Value: map[string]any{"c": make(chan int)},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := taggedContent(PushVersion, test.Value); !errors.Is(err, ErrMalformed) {
				t.Errorf("taggedContent() error = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

// TestSignTaggedNeedsAKey shows signing with no private key is refused rather than panicking.
func TestSignTaggedNeedsAKey(t *testing.T) {
	t.Parallel()
	p := samplePush()
	if _, err := signTagged(PushVersion, &p, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("signTagged() error = %v, want %v", err, ErrMalformed)
	}
}
