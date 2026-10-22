package witness

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/identity"
)

// keyEpoch is when every key a rotation test starts with began signing.
var keyEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// unrotatedKeyList is the signed key list of a witness that never rotated, holding the key whose
// seed is 32 bytes of 0x42, valid from pushEpoch, byte for byte as this package wrote it before a
// witness could rotate its key.
const unrotatedKeyList = `{"switchtender":"witness-keys/1",` +
	`"key_id":"sha256:3097e2dee2cb4a34b53840cdb705aed71067c36f68db0e0f559c3f3fa043315f",` +
	`"public_key":"IVL40Zt5HSRFMkLhXy6rbLfP+ntqXtMAl5YOBpiB2xI=",` +
	`"keys":[{"key_id":"sha256:3097e2dee2cb4a34b53840cdb705aed71067c36f68db0e0f559c3f3fa043315f",` +
	`"public_key":"IVL40Zt5HSRFMkLhXy6rbLfP+ntqXtMAl5YOBpiB2xI=",` +
	`"valid_from":"2026-10-21T12:00:00Z"}],` +
	`"sig":"Zbm/+Io21FWBe3RQsoXO8UVJrGTO6x2B/dXFnJlsYR9VDwXaqpkb7L8/09neujXrbcaTp9vgmJiwEcLky+fdBw=="}`

// fixedWitnessKey writes a witness key file whose seed is 32 copies of b into dir, dated at, and
// loads it.
func fixedWitnessKey(t *testing.T, dir string, b byte, at time.Time) identity.Identity {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"install_id": "",
		"seed": strings.Repeat(fmt.Sprintf("%02x", b), ed25519.SeedSize)})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	path := filepath.Join(dir, identity.WitnessFile)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	id, err := identity.LoadWitnessFile(dir)
	if err != nil {
		t.Fatalf("LoadWitnessFile() error = %v", err)
	}
	return id
}

// newKeyDirs returns a key directory holding a fixed witness key that began signing at keyEpoch,
// and a state directory apart from it that does not exist yet.
func newKeyDirs(t *testing.T, b byte) (keyDir, stateDir string) {
	t.Helper()
	base := t.TempDir()
	keyDir, stateDir = filepath.Join(base, "keys"), filepath.Join(base, "state")
	fixedWitnessKey(t, keyDir, b, keyEpoch)
	return keyDir, stateDir
}

// mustKeyring loads the keyring keyDir holds and fails the test otherwise.
func mustKeyring(t *testing.T, keyDir, stateDir string) *Keyring {
	t.Helper()
	ring, err := LoadKeyring(keyDir, stateDir)
	if err != nil {
		t.Fatalf("LoadKeyring() error = %v", err)
	}
	return ring
}

// mustRotate rotates the key keyDir holds at now and fails the test otherwise.
func mustRotate(t *testing.T, keyDir, stateDir string, now time.Time) Rotation {
	t.Helper()
	r, err := RotateKey(keyDir, stateDir, now)
	if err != nil {
		t.Fatalf("RotateKey() error = %v", err)
	}
	return r
}

// ringWitness starts a push witness over stateDir that signs with the keyring keyDir holds, as
// witness serve does.
func ringWitness(t *testing.T, keyDir, stateDir string, clock *testClock) *pushWitness {
	t.Helper()
	ring := mustKeyring(t, keyDir, stateDir)
	w := &pushWitness{id: ring.Current(), dir: stateDir, clock: clock}
	w.start(t, WithKeyring(ring))
	return w
}

// halt stops a ring witness, as a witness is stopped before its key is rotated.
func halt(w *pushWitness) {
	w.api.Close()
	w.svc.Close()
}

// rerun starts w again over the same state with the keyring keyDir now holds, as the restart after
// a rotation does.
func rerun(t *testing.T, w *pushWitness, keyDir string) {
	t.Helper()
	halt(w)
	ring := mustKeyring(t, keyDir, w.dir)
	w.id = ring.Current()
	w.start(t, WithKeyring(ring))
}

// servedKeyList fetches and decodes the key list the witness publishes.
func servedKeyList(t *testing.T, w *pushWitness) WitnessKeys {
	t.Helper()
	code, raw := w.get(t, "/.well-known/loomseal.json")
	if code != http.StatusOK {
		t.Fatalf("GET key list = %d %s", code, raw)
	}
	var keys WitnessKeys
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode key list: %v", err)
	}
	return keys
}

// signedList returns the key list a witness holding ring publishes.
func signedList(t *testing.T, ring *Keyring) WitnessKeys {
	t.Helper()
	return resign(t, ring.Keys(), ring.Current(), heldPrivate(ring.held)...)
}

// resign returns keys signed by current and endorsed by each retired key given.
func resign(t *testing.T, keys []WitnessKey, current identity.Identity,
	retired ...ed25519.PrivateKey) WitnessKeys {
	t.Helper()
	doc := WitnessKeys{Keys: keys}
	if err := SignWitnessKeys(&doc, current, retired...); err != nil {
		t.Fatalf("SignWitnessKeys() error = %v", err)
	}
	return doc
}

// copyKeyList returns a deep copy of k.
func copyKeyList(t *testing.T, k WitnessKeys) WitnessKeys {
	t.Helper()
	raw, err := json.Marshal(k)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var out WitnessKeys
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

// treeNames returns every file and directory under dir, relative to it, sorted.
func treeNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == dir {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, path); rel != "." {
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(names)
	return names
}

// keySecrets returns every form a private key in keyDir could leak in: each seed and each private
// key, in hex, in base64, and raw.
func keySecrets(t *testing.T, keyDir string) []string {
	t.Helper()
	var secrets []string
	err := filepath.WalkDir(keyDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var stored storedKey
		if json.Unmarshal(raw, &stored) != nil || stored.Seed == "" {
			return nil
		}
		seed, err := hex.DecodeString(stored.Seed)
		if err != nil {
			return err
		}
		priv := ed25519.NewKeyFromSeed(seed)
		secrets = append(secrets, stored.Seed, base64.StdEncoding.EncodeToString(seed),
			string(seed), hex.EncodeToString(priv), base64.StdEncoding.EncodeToString(priv),
			string(priv))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", keyDir, err)
	}
	return secrets
}

// TestARotatedListIsSignedByTheKeyAClientHolds pins rotation from a client's side: a client holding
// any earlier list the witness published accepts the list after one rotation or two on the word of
// the key that list named as current, and the enrollment answer carries the same endorsed list.
func TestARotatedListIsSignedByTheKeyAClientHolds(t *testing.T) {
	t.Parallel()
	keyDir, stateDir := newKeyDirs(t, 0x11)
	clock := &testClock{now: pushEpoch}
	w := ringWitness(t, keyDir, stateDir, clock)
	pr := newProducer(t, 4)
	answer, err := w.svc.Enroll(pr.enrollment(t, "lic_rotate", time.Time{}))
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	held, currents := []WitnessKeys{answer.Witness}, []string{w.id.KeyID()}
	for range 2 {
		halt(w)
		clock.Add(time.Hour)
		mustRotate(t, keyDir, stateDir, clock.Now())
		rerun(t, w, keyDir)
		held, currents = append(held, servedKeyList(t, w)), append(currents, w.id.KeyID())
	}
	again, err := w.svc.Enroll(pr.enrollment(t, "lic_rotate", time.Time{}))
	if err != nil {
		t.Fatalf("Enroll() again error = %v", err)
	}
	if signer, err := VerifyEnrollResponse(again); err != nil || signer != currents[2] {
		t.Fatalf("VerifyEnrollResponse() = %s, %v, want signed by %s", signer, err, currents[2])
	}
	if diff := cmp.Diff(held[2], again.Witness); diff != "" {
		t.Errorf("the enrollment answer carries another list (-served +answer):\n%s", diff)
	}
	if n := len(held[2].Endorsements); n != 2 {
		t.Errorf("the list after two rotations carries %d endorsements, want 2", n)
	}
	tests := []struct {
		// Prev indexes the list the client holds.
		Prev int
		// Next indexes the list the client fetches.
		Next int
	}{{ // Test 0: One rotation, a client holding the list it enrolled with.
		Prev: 0, Next: 1,
	}, { // Test 1: Two rotations, a client that missed the first.
		Prev: 0, Next: 2,
	}, { // Test 2: Two rotations, a client that took the first.
		Prev: 1, Next: 2,
	}, { // Test 3: No rotation since the client last fetched.
		Prev: 2, Next: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := VerifyKeyList(held[test.Prev], held[test.Next]); err != nil {
				t.Errorf("VerifyKeyList() error = %v, want the list accepted", err)
			}
			if got := held[test.Next].KeyID; got != currents[test.Next] {
				t.Errorf("the accepted list names %s current, want %s", got, currents[test.Next])
			}
		})
	}
}

// TestAnOldAttestationStillVerifiesAfterRotation pins that an attestation a key made before it
// retired keeps verifying against the published list, that the list says the key no longer speaks
// for the witness after it retired, and that the restarted witness reads the memory the old key
// signed and keeps countersigning the same install under the new key.
func TestAnOldAttestationStillVerifiesAfterRotation(t *testing.T) {
	t.Parallel()
	keyDir, stateDir := newKeyDirs(t, 0x22)
	clock := &testClock{now: pushEpoch}
	w := ringWitness(t, keyDir, stateDir, clock)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_old")
	code, first, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0))
	if code != http.StatusOK || first.ClaimAttestation == nil || first.HeadAttestation == nil {
		t.Fatalf("first push = %d %s, want two attestations", code, raw)
	}
	old := w.id
	halt(w)
	clock.Add(time.Hour)
	rot := mustRotate(t, keyDir, stateDir, clock.Now())
	rerun(t, w, keyDir)
	list := servedKeyList(t, w)
	if err := VerifyClaimAttestation(first.Head, *first.ClaimAttestation); err != nil {
		t.Errorf("VerifyClaimAttestation() error = %v", err)
	}
	if err := VerifyHeadAttestation(first.Root, first.Size, *first.HeadAttestation); err != nil {
		t.Errorf("VerifyHeadAttestation() error = %v", err)
	}
	for _, c := range []*Countersignature{first.ClaimAttestation, first.HeadAttestation} {
		at, err := time.Parse(time.RFC3339Nano, c.At)
		if err != nil {
			t.Fatalf("parse at %q: %v", c.At, err)
		}
		listed, ok := list.KeyAt(c.KeyID, at)
		if !ok || listed.PublicKey != c.PublicKey || listed.Retired != rot.Retired.Retired {
			t.Errorf("KeyAt(%s, %s) = %+v, %v, want the retired key with its retired time",
				c.KeyID, c.At, listed, ok)
		}
	}
	if _, ok := list.KeyAt(old.KeyID(), clock.Now()); ok {
		t.Error("KeyAt() names the retired key as the witness's word after it retired")
	}
	clock.Add(time.Minute)
	code, next, raw := w.pushHTTP(t, pr, pr.push(t, 2, 4, 2))
	if code != http.StatusOK || next.ClaimAttestation == nil {
		t.Fatalf("push after the rotation = %d %s, want it countersigned", code, raw)
	}
	at, err := time.Parse(time.RFC3339Nano, next.ClaimAttestation.At)
	if err != nil {
		t.Fatalf("parse at: %v", err)
	}
	if _, ok := list.KeyAt(next.ClaimAttestation.KeyID, at); !ok ||
		next.ClaimAttestation.KeyID != w.id.KeyID() {
		t.Errorf("the push after the rotation was countersigned by %s, want the current key %s",
			next.ClaimAttestation.KeyID, w.id.KeyID())
	}
	c, err := Load(filepath.Join(w.recordDir(pr), installCheckpointFile))
	if err != nil || c.PublicKey != w.id.PublicKeyHex() {
		t.Errorf("the memory after the push is signed by %v (%v), want the current key", c, err)
	}
}

// TestAPullMemoryOutlivesAKeyRotation pins that a witness watching a server starts after a rotation
// with the memory the old key signed, and that its first sweep signs that memory with the new key.
func TestAPullMemoryOutlivesAKeyRotation(t *testing.T) {
	t.Parallel()
	keyDir, stateDir := newKeyDirs(t, 0x23)
	feed := newFakeFeed(t, []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")})
	start := func() *Service {
		t.Helper()
		ring := mustKeyring(t, keyDir, stateDir)
		svc, err := NewService(ring.Current(), stateDir, time.Minute, []string{feed.srv.URL}, nil,
			testClient(), WithKeyring(ring))
		if err != nil {
			t.Fatalf("NewService() error = %v", err)
		}
		if err := svc.Start(); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		t.Cleanup(svc.Close)
		return svc
	}
	start().Close()
	path := filepath.Join(stateDir, StateFileName(feed.srv.URL))
	before, err := Load(path)
	if err != nil || before == nil {
		t.Fatalf("Load() = %v, %v, want the first sweep's memory", before, err)
	}
	rot := mustRotate(t, keyDir, stateDir, time.Now().Add(time.Second))
	start().Close()
	after, err := Load(path)
	if err != nil || after == nil {
		t.Fatalf("Load() after the rotation = %v, %v", after, err)
	}
	if got := KeyIDOf(after.PublicKey); got != rot.Current.KeyID {
		t.Errorf("the memory after the first sweep is signed by %s, want the new key %s", got,
			rot.Current.KeyID)
	}
	if after.LastBeat != before.LastBeat || after.LastHead != before.LastHead {
		t.Errorf("the memory moved from beat %d %s to beat %d %s across the restart",
			before.LastBeat, before.LastHead, after.LastBeat, after.LastHead)
	}
}

// TestARetryAcrossAKeyRotationAnswersTheSameBytes pins how a retried push is countersigned after a
// rotation: with the retired key that first answered it while the witness holds that key, so the
// answer is byte for byte the first one, and with the current key at the present time once it does
// not, so no key ever speaks for a time before it began.
func TestARetryAcrossAKeyRotationAnswersTheSameBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// DropRetired deletes the retired key's file before the restart.
		DropRetired bool
		// WantSame is whether the retry answers the first answer's bytes.
		WantSame bool
	}{{ // Test 0: The witness still holds the key that first answered.
		WantSame: true,
	}, { // Test 1: The retired key's file was deleted.
		DropRetired: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			keyDir, stateDir := newKeyDirs(t, 0x30+byte(testNum))
			clock := &testClock{now: pushEpoch}
			w := ringWitness(t, keyDir, stateDir, clock)
			pr := newProducer(t, 8)
			w.enroll(t, pr, "lic_retry_rotate")
			if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
				t.Fatalf("first push = %d %s", code, raw)
			}
			clock.Add(time.Minute)
			p := pr.push(t, 2, 4, 2)
			code, _, first := w.pushHTTP(t, pr, p)
			if code != http.StatusOK {
				t.Fatalf("second push = %d %s", code, first)
			}
			halt(w)
			clock.Add(time.Hour)
			rot := mustRotate(t, keyDir, stateDir, clock.Now())
			if test.DropRetired {
				name, _ := recordName(rot.Retired.KeyID)
				if err := os.Remove(filepath.Join(keyDir, retiredKeysDir, name+".json")); err != nil {
					t.Fatalf("Remove() error = %v", err)
				}
			}
			rerun(t, w, keyDir)
			clock.Add(time.Hour)
			code, answer, again := w.pushHTTP(t, pr, p)
			if code != http.StatusOK || answer.ClaimAttestation == nil {
				t.Fatalf("retry = %d %s", code, again)
			}
			if test.WantSame {
				if diff := cmp.Diff(string(first), string(again)); diff != "" {
					t.Errorf("the retry answered other bytes (-first +retry):\n%s", diff)
				}
				return
			}
			want, err := formatSealTime(clock.Now())
			if err != nil {
				t.Fatalf("formatSealTime() error = %v", err)
			}
			for _, c := range []*Countersignature{answer.ClaimAttestation, answer.HeadAttestation} {
				if c.KeyID != rot.Current.KeyID || c.At != want {
					t.Errorf("retry countersigned by %s at %s, want %s at %s", c.KeyID, c.At,
						rot.Current.KeyID, want)
				}
			}
			if err := VerifyClaimAttestation(answer.Head, *answer.ClaimAttestation); err != nil {
				t.Errorf("VerifyClaimAttestation() error = %v", err)
			}
			list := servedKeyList(t, w)
			if _, ok := list.KeyAt(rot.Current.KeyID, clock.Now()); !ok {
				t.Error("the list does not name the current key as the witness's word now")
			}
			if len(list.Endorsements) != 0 {
				t.Errorf("the list carries %d endorsements with no retired key held, want 0",
					len(list.Endorsements))
			}
		})
	}
}

// TestMemoryARetiredKeySignedAfterItRetiredIsRefused pins the limit on reading memory an earlier
// key signed: a retired key's signature counts only on memory observed before the key retired, and
// a key outside the history counts never, so a witness refuses to start on memory that is not its
// word.
func TestMemoryARetiredKeySignedAfterItRetiredIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Signer names who signs the rewritten memory: retired, current, or stranger.
		Signer string
		// Observed is when the memory says it was observed, relative to the rotation.
		Observed time.Duration
		// WantErr is whether the witness refuses to start.
		WantErr bool
	}{{ // Test 0: The retired key, on memory observed before it retired.
		Signer: "retired", Observed: -30 * time.Minute,
	}, { // Test 1: The retired key, on memory observed after it retired.
		Signer: "retired", Observed: 30 * time.Minute, WantErr: true,
	}, { // Test 2: The retired key, on memory observed the instant it retired.
		Signer: "retired", Observed: 0, WantErr: true,
	}, { // Test 3: The current key, on memory observed after the rotation.
		Signer: "current", Observed: 30 * time.Minute,
	}, { // Test 4: A key the witness never held.
		Signer: "stranger", Observed: -30 * time.Minute, WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			keyDir, stateDir := newKeyDirs(t, 0x40+byte(testNum))
			clock := &testClock{now: pushEpoch}
			w := ringWitness(t, keyDir, stateDir, clock)
			pr := newProducer(t, 4)
			w.enroll(t, pr, "lic_late")
			if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
				t.Fatalf("push = %d %s", code, raw)
			}
			retired := w.id
			halt(w)
			rotatedAt := pushEpoch.Add(time.Hour)
			mustRotate(t, keyDir, stateDir, rotatedAt)
			ring := mustKeyring(t, keyDir, stateDir)
			signer := map[string]identity.Identity{"retired": retired, "current": ring.Current(),
				"stranger": testIdentity(t)}[test.Signer]
			path := filepath.Join(w.recordDir(pr), installCheckpointFile)
			c, err := Load(path)
			if err != nil || c == nil {
				t.Fatalf("Load() = %v, %v", c, err)
			}
			c.ObservedAt = rotatedAt.Add(test.Observed)
			if err := Save(path, c, signer); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			clock.Set(rotatedAt.Add(time.Hour))
			svc, err := NewService(ring.Current(), stateDir, time.Minute, nil, nil, nil,
				WithServiceClock(clock.Now), WithAcceptEnrollments(DefaultMaxInstalls),
				WithEnrollmentPolicy(testPolicy), WithKeyring(ring))
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			err = svc.Start()
			if err == nil {
				svc.Close()
			}
			if (err != nil) != test.WantErr {
				t.Errorf("Start() error = %v, want error %v", err, test.WantErr)
			}
		})
	}
}

// TestAListSignedOnlyByANewKeyIsRefused pins the one rule that authenticates a rotation: the key
// the held list names as current must have signed the new list. A list that keeps the history but
// lacks that signature is refused, whoever made it.
func TestAListSignedOnlyByANewKeyIsRefused(t *testing.T) {
	t.Parallel()
	keyDir, stateDir := newKeyDirs(t, 0x33)
	list0 := signedList(t, mustKeyring(t, keyDir, stateDir))
	mustRotate(t, keyDir, stateDir, pushEpoch)
	ring1 := mustKeyring(t, keyDir, stateDir)
	list1 := signedList(t, ring1)
	mustRotate(t, keyDir, stateDir, pushEpoch.Add(time.Hour))
	ring2 := mustKeyring(t, keyDir, stateDir)
	list2 := signedList(t, ring2)
	k1 := ring2.held[0].priv
	forger := testIdentity(t)
	tests := []struct {
		// Prev is the list the client holds.
		Prev WitnessKeys
		// Next builds the list the client is offered.
		Next func(t *testing.T) WitnessKeys
	}{{ // Test 0: The rotated list with its endorsements stripped.
		Prev: list0,
		Next: func(t *testing.T) WitnessKeys {
			next := copyKeyList(t, list1)
			next.Endorsements = nil
			return next
		},
	}, { // Test 1: A forger's list that keeps the history and names the forger current.
		Prev: list0,
		Next: func(t *testing.T) WitnessKeys {
			keys := list1.Keys[:1]
			return resign(t, append(slices.Clone(keys), WitnessKeyOf(forger, pushEpoch)), forger)
		},
	}, { // Test 2: A list endorsed only by a key the held list already shows retired.
		Prev: list1,
		Next: func(t *testing.T) WitnessKeys {
			keys := slices.Clone(list2.Keys[:2])
			keys = append(keys, WitnessKeyOf(forger, pushEpoch.Add(time.Hour)))
			return resign(t, keys, forger, k1)
		},
	}, { // Test 3: The honest list with the endorsement of the held list's key stripped.
		Prev: list1,
		Next: func(t *testing.T) WitnessKeys {
			next := copyKeyList(t, list2)
			next.Endorsements = next.Endorsements[:1]
			return next
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			next := test.Next(t)
			if _, err := VerifyWitnessKeys(&next); err != nil {
				t.Fatalf("VerifyWitnessKeys() error = %v, want a list that verifies alone", err)
			}
			if err := VerifyKeyList(test.Prev, next); !errors.Is(err, ErrSignature) {
				t.Errorf("VerifyKeyList() error = %v, want %v", err, ErrSignature)
			}
		})
	}
}

// TestVerifyKeyListKeepsTheKeyHistory pins that a new list adds to the history and never rewrites
// it, even when every key that should sign it did.
func TestVerifyKeyListKeepsTheKeyHistory(t *testing.T) {
	t.Parallel()
	keyDir, stateDir := newKeyDirs(t, 0x66)
	mustRotate(t, keyDir, stateDir, pushEpoch)
	prev := signedList(t, mustKeyring(t, keyDir, stateDir))
	mustRotate(t, keyDir, stateDir, pushEpoch.Add(time.Hour))
	ring := mustKeyring(t, keyDir, stateDir)
	held := heldPrivate(ring.held)
	tests := []struct {
		// Edit changes the honest key list.
		Edit func(keys []WitnessKey) []WitnessKey
		// Endorsers index the held retired keys that endorse the edited list.
		Endorsers []int
		// Want is the error VerifyKeyList returns.
		Want error
	}{{ // Test 0: The honest list.
		Edit: func(keys []WitnessKey) []WitnessKey { return keys }, Endorsers: []int{0, 1},
	}, { // Test 1: The first key dropped.
		Edit: func(keys []WitnessKey) []WitnessKey { return keys[1:] }, Endorsers: []int{1},
		Want: ErrMalformed,
	}, { // Test 2: The held list's current key began signing at another time.
		Edit: func(keys []WitnessKey) []WitnessKey {
			keys[1].ValidFrom = stamp(pushEpoch.Add(time.Minute))
			return keys
		},
		Endorsers: []int{0, 1}, Want: ErrMalformed,
	}, { // Test 3: A key the held list shows retired retired at another time.
		Edit: func(keys []WitnessKey) []WitnessKey {
			keys[0].Retired = stamp(pushEpoch.Add(-time.Minute))
			return keys
		},
		Endorsers: []int{0, 1}, Want: ErrMalformed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var endorsers []ed25519.PrivateKey
			for _, i := range test.Endorsers {
				endorsers = append(endorsers, held[i])
			}
			next := resign(t, test.Edit(ring.Keys()), ring.Current(), endorsers...)
			err := VerifyKeyList(prev, next)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyKeyList() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestKeyListEndorsementsAreChecked pins that every endorsement a list carries is a retired listed
// key's signature over the bytes the current key signed, and that a list without endorsements still
// verifies on its own, since it is VerifyKeyList that asks for one.
func TestKeyListEndorsementsAreChecked(t *testing.T) {
	t.Parallel()
	keyDir, stateDir := newKeyDirs(t, 0x77)
	mustRotate(t, keyDir, stateDir, pushEpoch)
	mustRotate(t, keyDir, stateDir, pushEpoch.Add(time.Hour))
	list := signedList(t, mustKeyring(t, keyDir, stateDir))
	stranger := testIdentity(t)
	tests := []struct {
		// Mutate edits the list.
		Mutate func(k *WitnessKeys)
		// Want is the error VerifyWitnessKeys returns.
		Want error
	}{{ // Test 0: The list as signed.
		Mutate: func(*WitnessKeys) {},
	}, { // Test 1: Every endorsement stripped.
		Mutate: func(k *WitnessKeys) { k.Endorsements = nil },
	}, { // Test 2: One key's endorsement carrying the other's signature.
		Mutate: func(k *WitnessKeys) { k.Endorsements[0].Sig = k.Endorsements[1].Sig },
		Want:   ErrSignature,
	}, { // Test 3: An endorsement by a key the list does not name.
		Mutate: func(k *WitnessKeys) { k.Endorsements[0].KeyID = stranger.KeyID() },
		Want:   ErrMalformed,
	}, { // Test 4: An endorsement claiming the current key.
		Mutate: func(k *WitnessKeys) { k.Endorsements[0].KeyID = k.KeyID },
		Want:   ErrMalformed,
	}, { // Test 5: One key endorsing twice.
		Mutate: func(k *WitnessKeys) { k.Endorsements = append(k.Endorsements, k.Endorsements[0]) },
		Want:   ErrMalformed,
	}, { // Test 6: A retired time edited after signing.
		Mutate: func(k *WitnessKeys) { k.Keys[0].Retired = stamp(pushEpoch.Add(-time.Hour)) },
		Want:   ErrSignature,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			edited := copyKeyList(t, list)
			test.Mutate(&edited)
			_, err := VerifyWitnessKeys(&edited)
			if !errors.Is(err, test.Want) || (test.Want == nil) != (err == nil) {
				t.Errorf("VerifyWitnessKeys() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestSignWitnessKeysRefusesAListOutOfForm pins that the witness never publishes a key list a
// client would refuse: the keys must be in form with the signer as the one in use, and every
// endorsing key must be listed as retired, once.
func TestSignWitnessKeysRefusesAListOutOfForm(t *testing.T) {
	t.Parallel()
	keyDir, stateDir := newKeyDirs(t, 0x78)
	mustRotate(t, keyDir, stateDir, pushEpoch)
	ring := mustKeyring(t, keyDir, stateDir)
	k1 := ring.held[0].priv
	stranger := testIdentity(t)
	_, strangerKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	tests := []struct {
		// Keys are the keys the list names.
		Keys []WitnessKey
		// Signer signs the list.
		Signer identity.Identity
		// Retired endorse the list.
		Retired []ed25519.PrivateKey
	}{{ // Test 0: A signer the list does not name.
		Keys: ring.Keys(), Signer: stranger,
	}, { // Test 1: Two keys shown in use.
		Keys: append(ring.Keys(), WitnessKeyOf(stranger, pushEpoch)), Signer: ring.Current(),
	}, { // Test 2: An endorsing key the list does not name.
		Keys: ring.Keys(), Signer: ring.Current(), Retired: []ed25519.PrivateKey{strangerKey},
	}, { // Test 3: The same retired key endorsing twice.
		Keys: ring.Keys(), Signer: ring.Current(), Retired: []ed25519.PrivateKey{k1, k1},
	}, { // Test 4: A valid_from that is not a time.
		Keys: func() []WitnessKey {
			keys := ring.Keys()
			keys[1].ValidFrom = "soon"
			return keys
		}(),
		Signer: ring.Current(),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc := WitnessKeys{Keys: test.Keys}
			if err := SignWitnessKeys(&doc, test.Signer, test.Retired...); !errors.Is(err,
				ErrMalformed) {
				t.Errorf("SignWitnessKeys() error = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

// TestAnUnrotatedKeyListSerializesAsBefore pins that a witness that never rotated publishes its key
// list byte for byte as it did before rotation existed, signed directly and served from a keyring.
func TestAnUnrotatedKeyListSerializesAsBefore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// List builds the signed list.
		List func(t *testing.T) *WitnessKeys
	}{{ // Test 0: Signed directly.
		List: func(t *testing.T) *WitnessKeys {
			id := fixedWitnessKey(t, t.TempDir(), 0x42, keyEpoch)
			doc := &WitnessKeys{Keys: []WitnessKey{WitnessKeyOf(id, pushEpoch)}}
			if err := SignWitnessKeys(doc, id); err != nil {
				t.Fatalf("SignWitnessKeys() error = %v", err)
			}
			return doc
		},
	}, { // Test 1: Served by a witness over an unrotated keyring.
		List: func(t *testing.T) *WitnessKeys {
			keyDir := t.TempDir()
			fixedWitnessKey(t, keyDir, 0x42, pushEpoch)
			ring := mustKeyring(t, keyDir, t.TempDir())
			svc, err := NewService(ring.Current(), t.TempDir(), time.Minute, nil, nil, nil,
				WithAcceptEnrollments(1), WithEnrollmentPolicy(OpenEnrollment), WithKeyring(ring))
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			t.Cleanup(svc.Close)
			doc, err := svc.keyDocument()
			if err != nil {
				t.Fatalf("keyDocument() error = %v", err)
			}
			return doc
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(test.List(t))
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if diff := cmp.Diff(unrotatedKeyList, string(raw)); diff != "" {
				t.Errorf("the unrotated key list changed (-before +now):\n%s", diff)
			}
		})
	}
}

// TestRotationNeverPutsAKeyInTheStateDirectory pins custody: a rotation refuses a key directory
// that is, holds, or lies inside the state directory and writes nothing when it does, a witness
// refuses to start on a rotated key directory that overlaps its state, and a witness that rotated
// and kept running leaves no key material anywhere in its state directory.
func TestRotationNeverPutsAKeyInTheStateDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Layout returns the key and state directories under base.
		Layout func(t *testing.T, base string) (keyDir, stateDir string)
	}{{ // Test 0: The key directory is the state directory, the serve default.
		Layout: func(_ *testing.T, base string) (string, string) {
			return filepath.Join(base, "one"), filepath.Join(base, "one")
		},
	}, { // Test 1: The key directory inside the state directory.
		Layout: func(_ *testing.T, base string) (string, string) {
			return filepath.Join(base, "state", "keys"), filepath.Join(base, "state")
		},
	}, { // Test 2: The state directory inside the key directory.
		Layout: func(_ *testing.T, base string) (string, string) {
			return filepath.Join(base, "keys"), filepath.Join(base, "keys", "state")
		},
	}, { // Test 3: The state directory named through a link to the key directory.
		Layout: func(t *testing.T, base string) (string, string) {
			if runtime.GOOS == "windows" {
				t.Skip("symbolic links need privileges on Windows")
			}
			keys := filepath.Join(base, "keys")
			if err := os.MkdirAll(keys, 0o700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.Symlink(keys, filepath.Join(base, "link")); err != nil {
				t.Fatalf("Symlink() error = %v", err)
			}
			return keys, filepath.Join(base, "link")
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			keyDir, stateDir := test.Layout(t, base)
			fixedWitnessKey(t, keyDir, 0x50+byte(testNum), keyEpoch)
			before := treeNames(t, base)
			if _, err := RotateKey(keyDir, stateDir, pushEpoch); err == nil {
				t.Fatal("RotateKey() = nil error, want the overlapping key directory refused")
			}
			if diff := cmp.Diff(before, treeNames(t, base)); diff != "" {
				t.Errorf("a refused rotation wrote (-before +after):\n%s", diff)
			}
			mustRotate(t, keyDir, filepath.Join(t.TempDir(), "elsewhere"), pushEpoch)
			if _, err := LoadKeyring(keyDir, stateDir); err == nil {
				t.Error("LoadKeyring() = nil error, want a rotated key directory that overlaps " +
					"the state refused")
			}
		})
	}
	t.Run("apart", func(t *testing.T) {
		t.Parallel()
		keyDir, stateDir := newKeyDirs(t, 0x5f)
		clock := &testClock{now: pushEpoch}
		w := ringWitness(t, keyDir, stateDir, clock)
		pr := newProducer(t, 8)
		w.enroll(t, pr, "lic_custody")
		for i, rotate := range []bool{false, true, true} {
			if rotate {
				halt(w)
				clock.Add(time.Hour)
				mustRotate(t, keyDir, stateDir, clock.Now())
				rerun(t, w, keyDir)
			}
			clock.Add(time.Minute)
			beat, size := int64(i+1), int64(2*(i+1))
			if code, _, raw := w.pushHTTP(t, pr, pr.push(t, beat, size, size-2)); code !=
				http.StatusOK {
				t.Fatalf("push %d = %d %s", beat, code, raw)
			}
		}
		halt(w)
		secrets := keySecrets(t, keyDir)
		if len(secrets) != 3*6 {
			t.Fatalf("found %d key secret forms in the key directory, want 18 from three keys",
				len(secrets))
		}
		for name, content := range snapshotDir(t, stateDir) {
			for _, secret := range secrets {
				if strings.Contains(content, secret) {
					t.Errorf("state file %s holds key material", name)
				}
			}
		}
		for _, name := range treeNames(t, stateDir) {
			base := filepath.Base(name)
			if base == identity.WitnessFile || base == keyHistoryFile || base == retiredKeysDir ||
				base == nextKeyDir {
				t.Errorf("the state directory holds %s", name)
			}
		}
	})
}

// TestLoadKeyringFinishesAnInterruptedKeyRotation pins what a rotation stopped partway leaves:
// before the history names the new key the old key stays current and the next rotation starts
// clean, after it the next load moves the new key into place, and a history whose current key is
// gone is refused rather than answered with a key nothing endorses.
func TestLoadKeyringFinishesAnInterruptedKeyRotation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Stop leaves the key directory as a rotation stopped partway would and returns the key id
		// the next load must make current, empty when it must refuse.
		Stop func(t *testing.T, keyDir, stateDir string) string
	}{{ // Test 0: Stopped after the history named the new key, before it moved into place.
		Stop: func(t *testing.T, keyDir, stateDir string) string {
			rot := mustRotate(t, keyDir, stateDir, pushEpoch)
			next := filepath.Join(keyDir, nextKeyDir)
			if err := os.MkdirAll(next, 0o700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			current := filepath.Join(keyDir, identity.WitnessFile)
			if err := os.Rename(current, filepath.Join(next, identity.WitnessFile)); err != nil {
				t.Fatalf("Rename() error = %v", err)
			}
			name, _ := recordName(rot.Retired.KeyID)
			raw, err := os.ReadFile(filepath.Join(keyDir, retiredKeysDir, name+".json"))
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if err := os.WriteFile(current, raw, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			return rot.Current.KeyID
		},
	}, { // Test 1: Stopped after the new key was made, before the history named it.
		Stop: func(t *testing.T, keyDir, _ string) string {
			old, err := identity.LoadWitnessFile(keyDir)
			if err != nil {
				t.Fatalf("LoadWitnessFile() error = %v", err)
			}
			if _, err := identity.LoadWitnessFile(filepath.Join(keyDir, nextKeyDir)); err != nil {
				t.Fatalf("LoadWitnessFile() error = %v", err)
			}
			return old.KeyID()
		},
	}, { // Test 2: The current key file is gone after a rotation.
		Stop: func(t *testing.T, keyDir, stateDir string) string {
			mustRotate(t, keyDir, stateDir, pushEpoch)
			if err := os.Remove(filepath.Join(keyDir, identity.WitnessFile)); err != nil {
				t.Fatalf("Remove() error = %v", err)
			}
			return ""
		},
	}, { // Test 3: The current key file holds another key after a rotation.
		Stop: func(t *testing.T, keyDir, stateDir string) string {
			mustRotate(t, keyDir, stateDir, pushEpoch)
			fixedWitnessKey(t, keyDir, 0x7f, keyEpoch)
			return ""
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			keyDir, stateDir := newKeyDirs(t, 0x60+byte(testNum))
			want := test.Stop(t, keyDir, stateDir)
			ring, err := LoadKeyring(keyDir, stateDir)
			if want == "" {
				if err == nil {
					t.Error("LoadKeyring() = nil error, want the missing current key refused")
				}
				if testNum == 2 {
					if _, serr := os.Stat(filepath.Join(keyDir, identity.WitnessFile)); serr == nil {
						t.Error("LoadKeyring() made a new key in place of the missing one")
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadKeyring() error = %v", err)
			}
			if got := ring.Current().KeyID(); got != want {
				t.Errorf("current key = %s, want %s", got, want)
			}
			if again := mustKeyring(t, keyDir, stateDir); again.Current().KeyID() != want {
				t.Errorf("a second load reads %s, want %s", again.Current().KeyID(), want)
			}
			rot := mustRotate(t, keyDir, stateDir, pushEpoch.Add(time.Hour))
			if rot.Retired.KeyID != want {
				t.Errorf("the next rotation retired %s, want %s", rot.Retired.KeyID, want)
			}
			if _, serr := os.Stat(filepath.Join(keyDir, nextKeyDir)); !errors.Is(serr,
				fs.ErrNotExist) {
				t.Errorf("the staging directory is left behind: %v", serr)
			}
		})
	}
}

// TestRotateKeyRefusesWhatItCannotRotate pins the refusals that leave the key directory exactly as
// they found it: no key to rotate, a clock not past when the current key began, and a retired key
// another account can read.
func TestRotateKeyRefusesWhatItCannotRotate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Prepare readies the key directory.
		Prepare func(t *testing.T, keyDir, stateDir string)
		// Now is when the rotation runs.
		Now time.Time
	}{{ // Test 0: No key to rotate.
		Prepare: func(t *testing.T, keyDir, _ string) {
			if err := os.Remove(filepath.Join(keyDir, identity.WitnessFile)); err != nil {
				t.Fatalf("Remove() error = %v", err)
			}
		},
		Now: pushEpoch,
	}, { // Test 1: A clock at the instant the current key began.
		Prepare: func(*testing.T, string, string) {}, Now: keyEpoch,
	}, { // Test 2: A retired key file another account can read.
		Prepare: func(t *testing.T, keyDir, stateDir string) {
			if runtime.GOOS == "windows" {
				t.Skip("file modes do not say who can read a file on Windows")
			}
			rot := mustRotate(t, keyDir, stateDir, pushEpoch)
			name, _ := recordName(rot.Retired.KeyID)
			if err := os.Chmod(filepath.Join(keyDir, retiredKeysDir, name+".json"),
				0o644); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
		},
		Now: pushEpoch.Add(time.Hour),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			keyDir, stateDir := newKeyDirs(t, 0x70+byte(testNum))
			test.Prepare(t, keyDir, stateDir)
			before, names := snapshotDir(t, keyDir), treeNames(t, keyDir)
			if _, err := RotateKey(keyDir, stateDir, test.Now); err == nil {
				t.Fatal("RotateKey() = nil error, want a refusal")
			}
			if diff := cmp.Diff(before, snapshotDir(t, keyDir), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("a refused rotation changed the key directory (-before +after):\n%s", diff)
			}
			if diff := cmp.Diff(names, treeNames(t, keyDir)); diff != "" {
				t.Errorf("a refused rotation made (-before +after):\n%s", diff)
			}
		})
	}
}
