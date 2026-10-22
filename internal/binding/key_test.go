package binding

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// testSeed returns a deterministic seed of n bytes whose bytes start at first.
func testSeed(n int, first byte) []byte {
	seed := make([]byte, n)
	for i := range seed {
		seed[i] = first + byte(i)
	}
	return seed
}

// testKey returns a key derived from a fixed seed starting at first.
func testKey(t *testing.T, first byte) *Key {
	t.Helper()
	k, err := DeriveKey(testSeed(32, first))
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	return k
}

// TestDeriveKeyRefusesAShortSeed pins the seed length DeriveKey accepts.
func TestDeriveKeyRefusesAShortSeed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want    error
		SeedLen int
	}{{ // Test 0: No seed.
		SeedLen: 0, Want: ErrShortSeed,
	}, { // Test 1: One byte short.
		SeedLen: 31, Want: ErrShortSeed,
	}, { // Test 2: An Ed25519 seed.
		SeedLen: 32, Want: nil,
	}, { // Test 3: A longer seed.
		SeedLen: 64, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			k, err := DeriveKey(testSeed(test.SeedLen, 1))
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if (k == nil) != (test.Want != nil) {
				t.Fatalf("key = %v with error %v", k, err)
			}
		})
	}
}

// TestDeriveKeyIsHKDFWithTheBindingInfo recomputes RFC 5869 HKDF-SHA256 by hand, with an empty salt
// and the info string spelled out here, so a change to the salt, the info string, the hash, or the
// length moves the derived key and fails this test.
func TestDeriveKeyIsHKDFWithTheBindingInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Seed []byte
	}{{ // Test 0: A 32-byte seed.
		Seed: testSeed(32, 0),
	}, { // Test 1: A different 32-byte seed.
		Seed: testSeed(32, 100),
	}, { // Test 2: A 64-byte seed.
		Seed: testSeed(64, 7),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			extract := hmac.New(sha256.New, make([]byte, sha256.Size))
			extract.Write(test.Seed)
			prk := extract.Sum(nil)
			expand := hmac.New(sha256.New, prk)
			expand.Write([]byte("switchtender approval binding v1"))
			expand.Write([]byte{1})
			want := expand.Sum(nil)
			k, err := DeriveKey(test.Seed)
			if err != nil {
				t.Fatalf("derive key: %v", err)
			}
			got := hex.EncodeToString(k.mac[:])
			if diff := cmp.Diff(hex.EncodeToString(want), got); diff != "" {
				t.Errorf("key mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDeriveKeyIsDeterministic pins that one seed always derives one key and two seeds two keys,
// since every replica must recompute every seal another replica made.
func TestDeriveKeyIsDeterministic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		A, B      []byte
		WantEqual bool
	}{{ // Test 0: The same seed twice.
		A: testSeed(32, 3), B: testSeed(32, 3), WantEqual: true,
	}, { // Test 1: Seeds differing in one byte.
		A: testSeed(32, 3), B: append(testSeed(31, 3), 0), WantEqual: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, err := DeriveKey(test.A)
			if err != nil {
				t.Fatalf("derive a: %v", err)
			}
			b, err := DeriveKey(test.B)
			if err != nil {
				t.Fatalf("derive b: %v", err)
			}
			if got := a.mac == b.mac; got != test.WantEqual {
				t.Errorf("keys equal = %t, want %t", got, test.WantEqual)
			}
		})
	}
}

// TestAdoptionIDIsKeyedAndFixed recomputes the adoption record's id by hand, an HMAC-SHA256 under
// the key over the marker string spelled out here, so a change to the string, the hash, the prefix,
// or the width moves the id and fails this test. One key always names one id, so every replica of
// an install collides on the same record, and another key names another id, so the record of one
// install says nothing about another and nobody without the key can name it.
func TestAdoptionIDIsKeyedAndFixed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// A is the seed of the key whose id is recomputed by hand.
		A []byte
		// B is the seed of a second key whose id is compared with A's.
		B []byte
		// WantEqual reports whether the two keys name the same id.
		WantEqual bool
	}{{ // Test 0: One seed derived twice names one id.
		A: testSeed(32, 5), B: testSeed(32, 5), WantEqual: true,
	}, { // Test 1: Seeds differing in one byte name two ids.
		A: testSeed(32, 5), B: append(testSeed(31, 5), 0), WantEqual: false,
	}, { // Test 2: A longer seed.
		A: testSeed(64, 40), B: testSeed(64, 41), WantEqual: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, err := DeriveKey(test.A)
			if err != nil {
				t.Fatalf("derive a: %v", err)
			}
			b, err := DeriveKey(test.B)
			if err != nil {
				t.Fatalf("derive b: %v", err)
			}
			m := hmac.New(sha256.New, a.mac[:])
			m.Write([]byte("switchtender adoption marker v1"))
			want := "aud_" + hex.EncodeToString(m.Sum(nil))[:32]
			if diff := cmp.Diff(want, a.AdoptionID()); diff != "" {
				t.Errorf("adoption id mismatch (-want +got):\n%s", diff)
			}
			if got := a.AdoptionID() == b.AdoptionID(); got != test.WantEqual {
				t.Errorf("ids equal = %t, want %t", got, test.WantEqual)
			}
			if strings.Contains(a.AdoptionID(), hex.EncodeToString(a.mac[:])) {
				t.Errorf("adoption id %s carries the key's bytes", a.AdoptionID())
			}
		})
	}
}

// TestKeyNeverPrintsItsBytes pins that a key formatted under any verb, or marshaled, carries none
// of its bytes.
func TestKeyNeverPrintsItsBytes(t *testing.T) {
	t.Parallel()
	k := testKey(t, 9)
	secretHex := hex.EncodeToString(k.mac[:])
	tests := []struct {
		Out string
	}{
		{Out: fmt.Sprintf("%v", k)},   // Test 0: Pointer, %v.
		{Out: fmt.Sprintf("%+v", k)},  // Test 1: Pointer, %+v.
		{Out: fmt.Sprintf("%#v", k)},  // Test 2: Pointer, %#v.
		{Out: fmt.Sprintf("%x", k)},   // Test 3: Pointer, %x.
		{Out: fmt.Sprintf("%d", k)},   // Test 4: Pointer, %d.
		{Out: fmt.Sprintf("%v", *k)},  // Test 5: Value, %v.
		{Out: fmt.Sprintf("%#v", *k)}, // Test 6: Value, %#v.
		{Out: fmt.Sprintf("%s", *k)},  // Test 7: Value, %s.
		{Out: fmt.Sprint(k)},          // Test 8: Print.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff("binding.Key(redacted)", test.Out); diff != "" {
				t.Errorf("formatted key mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(strings.ToLower(test.Out), secretHex) {
				t.Errorf("formatted key carries its bytes: %s", test.Out)
			}
		})
	}
	for _, v := range []any{k, *k, struct{ K *Key }{K: k}} {
		raw, err := json.Marshal(v)
		if !errors.Is(err, ErrSerialize) {
			t.Errorf("marshal %T = %s, %v, want %v", v, raw, err, ErrSerialize)
		}
	}
}
