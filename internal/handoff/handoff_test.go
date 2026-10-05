package handoff

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
)

// testKeys are generated once for the whole file. Generating is cheap, but sharing them keeps every
// table naming the same two pools.
//
//nolint:gochecknoglobals // Not modified, simplifies testing.
var testKeys = sync.OnceValues(func() (*PrivateKey, *PrivateKey) {
	a, err := GenerateKey()
	if err != nil {
		panic("generate pool a key: " + err.Error())
	}
	b, err := GenerateKey()
	if err != nil {
		panic("generate pool b key: " + err.Error())
	}
	return a, b
})

// testBinding is the claim most tests seal for.
func testBinding() Binding {
	return Binding{RunID: "run_alpha", Lease: "lease-for-the-first-claim", Owner: "relay-a"}
}

// testPayload returns a payload carrying one of each kind of secret, with values a test can look
// for anywhere they must not be.
func testPayload(runID string) *Payload {
	ssh := NewCredential(&credential.Credential{
		ID: "cred_ssh", Name: "fleet key", Kind: credential.KindSSHKey,
		Secret: "sealed-at-rest-form-must-not-travel", Source: "vault",
		Settings: map[string]string{"user": "deploy"},
	}, "-----BEGIN OPENSSH PRIVATE KEY-----\nkey-material-one\n-----END OPENSSH PRIVATE KEY-----\n")
	oidc := NewCredential(&credential.Credential{ID: "cred_oidc", Kind: credential.KindAWSOIDC}, "")
	oidc.Token = "eyJ.minted-identity-token.sig"
	return &Payload{
		RunID: runID, DryRun: true, CredentialIDs: []string{"cred_ssh", "cred_oidc"},
		Credentials: []*Credential{ssh, oidc},
		Types: []*credential.CredentialType{{ID: "ctype_a", Name: "Datadog",
			Fields: []credential.Field{{Name: "api_key", Secret: true}}}},
		Answers: map[string]string{"db_password": "survey-answer-value"},
	}
}

// secretsOf lists the values testPayload carries that no record may hold.
func secretsOf() []string {
	return []string{"key-material-one", "minted-identity-token", "survey-answer-value",
		"sealed-at-rest-form"}
}

// TestSealOpensOnlyForItsClaim pins the binding: a delivery opens for the run, lease, and worker it
// was sealed for, and for nothing else. Each altered field stands for a replay or a substitution: a
// delivery handed to another run's execution, carried over from an earlier claim of the same run,
// presented by another worker, or edited on the way.
func TestSealOpensOnlyForItsClaim(t *testing.T) {
	t.Parallel()
	keyA, keyB := testKeys()
	tests := []struct {
		Ring   func() *KeyRing
		Bind   func(Binding) Binding
		Tamper func(*Envelope)
		Want   error
	}{{ // Test 0: The claim it was sealed for opens it.
		Want: nil,
	}, { // Test 1: Another run's execution cannot take it.
		Bind: func(b Binding) Binding { b.RunID = "run_beta"; return b }, Want: ErrBinding,
	}, { // Test 2: A later claim of the same run, under a new lease, cannot take it.
		Bind: func(b Binding) Binding { b.Lease = "lease-for-the-second-claim"; return b },
		Want: ErrBinding,
	}, { // Test 3: Another worker of the same pool cannot take it.
		Bind: func(b Binding) Binding { b.Owner = "relay-b"; return b }, Want: ErrBinding,
	}, { // Test 4: Renaming the run on the envelope to match another claim does not help.
		Bind:   func(b Binding) Binding { b.RunID = "run_beta"; return b },
		Tamper: func(e *Envelope) { e.RunID = "run_beta" }, Want: ErrBinding,
	}, { // Test 5: The pool it names is authenticated.
		Tamper: func(e *Envelope) { e.Pool = "prod" }, Want: ErrBinding,
	}, { // Test 6: The delivery id is authenticated, so a replay cannot pose as a fresh delivery.
		Tamper: func(e *Envelope) { e.DeliveryID = strings.Repeat("0", 32) }, Want: ErrBinding,
	}, { // Test 7: One flipped ciphertext bit fails.
		Tamper: func(e *Envelope) { e.Ciphertext[0] ^= 1 }, Want: ErrBinding,
	}, { // Test 8: A substituted ephemeral key fails.
		Tamper: func(e *Envelope) { e.Enc[0] ^= 1 }, Want: ErrBinding,
	}, { // Test 9: Pointing the key id at another key the worker holds does not open it.
		Ring:   func() *KeyRing { return ring(t, keyA, keyB) },
		Tamper: func(e *Envelope) { e.KeyID = keyB.ID() }, Want: ErrBinding,
	}, { // Test 10: A format this build does not know is refused before any decryption.
		Tamper: func(e *Envelope) { e.Version = 2 }, Want: ErrEnvelope,
	}, { // Test 11: Another suite is refused, so an envelope cannot pick a weaker one.
		Tamper: func(e *Envelope) { e.Suite = "hpke-base-x25519-hkdf-sha256-aes128gcm" },
		Want:   ErrEnvelope,
	}, { // Test 12: A worker holding no claim lease has nothing to open it under.
		Bind: func(b Binding) Binding { b.Lease = ""; return b }, Want: ErrBinding,
	}, { // Test 13: An oversized ciphertext is refused without being decrypted.
		Tamper: func(e *Envelope) { e.Ciphertext = make([]byte, maxSealed+1) }, Want: ErrEnvelope,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			env, err := Seal(keyA.Public(), "dmz", testBinding(), testPayload("run_alpha"))
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			if test.Tamper != nil {
				test.Tamper(env)
			}
			b := testBinding()
			if test.Bind != nil {
				b = test.Bind(b)
			}
			r := ring(t, keyA)
			if test.Ring != nil {
				r = test.Ring()
			}
			got, err := Open(r, b, env)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Open() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				if got != nil {
					t.Errorf("Open() returned a payload beside its error")
				}
				return
			}
			want := testPayload("run_alpha")
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("payload mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAnotherPoolsWorkerCannotOpen pins the pool boundary. A pool's workers hold only their own
// key, so a delivery sealed for one pool is ciphertext to every other, whichever key id it names.
func TestAnotherPoolsWorkerCannotOpen(t *testing.T) {
	t.Parallel()
	keyA, keyB := testKeys()
	tests := []struct {
		Tamper func(*Envelope)
		Want   error
	}{{ // Test 0: Pool b's worker does not hold pool a's key.
		Want: ErrUnknownKey,
	}, { // Test 1: Relabeling the envelope with pool b's key id does not make pool b's key open it.
		Tamper: func(e *Envelope) { e.KeyID = keyB.ID() }, Want: ErrBinding,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			env, err := Seal(keyA.Public(), "pool-a", testBinding(), testPayload("run_alpha"))
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			if test.Tamper != nil {
				test.Tamper(env)
			}
			got, err := Open(ring(t, keyB), testBinding(), env)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Open() with pool b's key error = %v, want %v", err, test.Want)
			}
			if got != nil {
				t.Errorf("pool b's worker opened pool a's delivery")
			}
		})
	}
}

// TestSealRefusesWhatItCannotBind pins that nothing is sealed without a complete binding. A
// delivery sealed with no lease or no worker would open for any claim of the run, which is the
// replay the binding exists to stop.
func TestSealRefusesWhatItCannotBind(t *testing.T) {
	t.Parallel()
	keyA, _ := testKeys()
	tests := []struct {
		Key     *PublicKey
		Binding Binding
		Payload *Payload
		Want    error
	}{{ // Test 0: No key to seal to.
		Key: nil, Binding: testBinding(), Payload: testPayload("run_alpha"), Want: ErrSeal,
	}, { // Test 1: No payload.
		Key: keyA.Public(), Binding: testBinding(), Payload: nil, Want: ErrSeal,
	}, { // Test 2: No lease.
		Key: keyA.Public(), Binding: Binding{RunID: "run_alpha", Owner: "relay-a"},
		Payload: testPayload("run_alpha"), Want: ErrSeal,
	}, { // Test 3: No worker.
		Key: keyA.Public(), Binding: Binding{RunID: "run_alpha", Lease: "l"},
		Payload: testPayload("run_alpha"), Want: ErrSeal,
	}, { // Test 4: A payload opened for another run.
		Key: keyA.Public(), Binding: testBinding(), Payload: testPayload("run_beta"), Want: ErrSeal,
	}, { // Test 5: A complete binding seals.
		Key: keyA.Public(), Binding: testBinding(), Payload: testPayload("run_alpha"), Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			env, err := Seal(test.Key, "dmz", test.Binding, test.Payload)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Seal() error = %v, want %v", err, test.Want)
			}
			if err == nil && (env.KeyID != test.Key.ID() || env.RunID != test.Binding.RunID ||
				len(env.DeliveryID) != 32) {
				t.Errorf("envelope = %+v, want it to name the key, the run, and a delivery id", env)
			}
		})
	}
}

// TestEveryDeliveryIsDistinct pins that two seals of the same payload for the same claim share no
// ciphertext and no delivery id, which is what lets a worker refuse a delivery it has already
// opened without refusing a legitimate second delivery.
func TestEveryDeliveryIsDistinct(t *testing.T) {
	t.Parallel()
	keyA, _ := testKeys()
	first, err := Seal(keyA.Public(), "dmz", testBinding(), testPayload("run_alpha"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	second, err := Seal(keyA.Public(), "dmz", testBinding(), testPayload("run_alpha"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if first.DeliveryID == second.DeliveryID {
		t.Errorf("two deliveries share id %s", first.DeliveryID)
	}
	if string(first.Enc) == string(second.Enc) ||
		string(first.Ciphertext) == string(second.Ciphertext) {
		t.Errorf("two deliveries share an ephemeral key or ciphertext")
	}
}

// TestNothingSecretTravelsInTheClear pins that the envelope, as it crosses the wire, carries no
// secret in any form a reader of a relay log would see: not the value, not the token, not the
// answer, and not the at-rest sealed form, which never leaves the control node at all.
func TestNothingSecretTravelsInTheClear(t *testing.T) {
	t.Parallel()
	keyA, _ := testKeys()
	env, err := Seal(keyA.Public(), "dmz", testBinding(), testPayload("run_alpha"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	wire, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, secret := range append(secretsOf(), testBinding().Lease) {
		if strings.Contains(string(wire), secret) {
			t.Errorf("the envelope on the wire carries %q", secret)
		}
	}
	opened, err := Open(ring(t, keyA), testBinding(), env)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := opened.Credential("cred_ssh").Record.Secret; got != "" {
		t.Errorf("the at-rest sealed form traveled to the worker: %q", got)
	}
	if got := opened.Credential("cred_ssh").Record.Source; got != "" {
		t.Errorf("the source traveled, so a worker would resolve an already resolved value: %q",
			got)
	}
}

// TestEnvelopeWipe pins that a wiped envelope keeps no ciphertext.
func TestEnvelopeWipe(t *testing.T) {
	t.Parallel()
	keyA, _ := testKeys()
	env, err := Seal(keyA.Public(), "dmz", testBinding(), testPayload("run_alpha"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	ct := env.Ciphertext
	env.Wipe()
	if env.Ciphertext != nil || env.Enc != nil {
		t.Errorf("Wipe() left the ciphertext referenced")
	}
	for i, b := range ct {
		if b != 0 {
			t.Fatalf("Wipe() left byte %d of the ciphertext unzeroed", i)
		}
	}
	(*Envelope)(nil).Wipe()
}

// ring builds a key ring from keys, failing the test on a refusal.
func ring(t *testing.T, keys ...*PrivateKey) *KeyRing {
	t.Helper()
	r, err := NewKeyRing(keys...)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	return r
}
